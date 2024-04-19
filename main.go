// webservice
// image upload
// pg dependency
// rabbitmq

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"
	"github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/middleware/logger"
	"github.com/kataras/iris/v12/middleware/recover"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog/log"
)

func main() {
	// Database
	postgresUsername := os.Getenv("POSTGRES_USERNAME")
	postgresPassword := os.Getenv("POSTGRES_PASSWORD")
	postgresHost := os.Getenv("POSTGRES_HOST")
	minioEndpoint := os.Getenv("S3_ENDPOINT")
	minioAccessKeyID := os.Getenv("S3_ACCESSKEY")
	minioSecretAccessKey := os.Getenv("S3_SECRET")
	mqUsername := os.Getenv("MQ_USERNAME")
	mqPassword := os.Getenv("MQ_PASSWORD")
	mqEndpoint := os.Getenv("MQ_ENDPOINT")

	if postgresUsername == "" || postgresPassword == "" || postgresHost == "" ||
		minioEndpoint == "" || minioAccessKeyID == "" || minioSecretAccessKey == "" ||
		mqUsername == "" || mqPassword == "" || mqEndpoint == "" {
		log.Fatal().Msg("You must set the following environment variables: POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST," +
			" S3_ENDPOINT, S3_ACCESSKEY, S3_SECRET, MQ_USERNAME, MQ_PASSWORD, MQ_ENDPOINT")
	}

	db, err := setUpDb(postgresHost, postgresUsername, postgresPassword)
	defer db.Close()
	failOnError(err, "Failed to connect to postgres")

	bucketName, minioClient, err := setupObjectStorage(err, minioEndpoint, minioAccessKeyID, minioSecretAccessKey)
	failOnError(err, "Failed to setup object storage")

	conn, ch, q, err := setupMq(mqUsername, mqPassword, mqEndpoint)
	defer conn.Close()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to setup mq connection")
	}

	// Web Service
	app := iris.New()
	app.Logger().SetLevel("debug")

	// Recover panics
	app.Use(recover.New())
	app.Use(logger.New())

	// Method: GET
	// Resource http://localhost:8080
	app.Handle("GET", "/", doHttpGet(db))
	app.Handle("POST", "/", doHttpPost(minioClient, bucketName, db, ch, q))

	err = app.Run(iris.Addr("localhost:8080"), iris.WithoutServerError(iris.ErrServerClosed))
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to start server")
	}
}

func doHttpPost(minioClient *minio.Client, bucketName string, db *pg.DB, ch *amqp.Channel, q *amqp.Queue) func(ctx iris.Context) {
	return func(ctx iris.Context) {
		var image Image

		// Receive the incoming file
		// See also: https://github.com/kataras/iris/blob/c4843a4d82aae53518bb7c247923007d1d99893c/_examples/file-server/upload-file/main.go
		file, info, err := ctx.FormFile("image")

		if err != nil {
			log.Error().Err(err).Msg("could not get image from HTTP request")
			ctx.StatusCode(iris.StatusInternalServerError)
			_, _ = ctx.HTML("Fileupload: Error while uploading: " + err.Error() + "\n" + info.Filename)
			return
		}

		defer file.Close()

		// Upload the zip file to MinIO
		objectName := info.Filename
		fileName := info.Filename

		objectSize := info.Size
		objectReader := bufio.NewReader(file)

		log.Info().Msgf("Fileupload: Receiving file with path: %s", fileName)

		// Upload the zip file with FPutObject
		n, err := minioClient.PutObject(ctx, bucketName, objectName, objectReader, objectSize, minio.PutObjectOptions{})
		if err != nil {
			log.Error().Err(err).Msg("failed to put object to s3")
			ctx.StatusCode(iris.StatusInternalServerError)
			return
		}

		log.Info().Msgf("MinIO: Successfully uploaded %s of size %v", objectName, n)

		image.Name = fileName
		image.StorageLocation = bucketName + "/" + fileName

		// Write to the DB
		_, err = db.Model(&image).Insert()
		if err != nil {
			log.Error().Err(err).Msg("failed to save image to db")
			ctx.StatusCode(iris.StatusInternalServerError)
			_, _ = ctx.Writef("PG database error: " + err.Error())
			return
		}

		log.Info().Msg("successfully persisted event to db")

		body, err := json.Marshal(image)
		if (err) != nil {
			panic(err)
		}

		// Write to the message queue
		err = ch.PublishWithContext(
			ctx,
			"",     // exchange
			q.Name, // routing key
			false,  // mandatory
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
			},
		)

		log.Info().Msg("Successfully sent event to MQ")

		_, err = ctx.Writef("Success: %s %s", image.Name, image.StorageLocation)
		if err != nil {
			log.Error().Err(err).Msg("error'd while sending reply")
		}
	}
}

func doHttpGet(db *pg.DB) func(ctx iris.Context) {
	return func(ctx iris.Context) {
		var images []Image
		err := db.Model(&images).Order("id ASC").Select()
		if err != nil {
			log.Err(err).Msg("could not load images from db")
			ctx.StatusCode(iris.StatusInternalServerError)
			return
		}

		if images == nil {
			images = []Image{}
		}

		err = ctx.JSON(images)
		if err != nil {
			log.Error().Err(err).Msg("Could not serialize JSON response")
		}
	}
}

func setupMq(mqUsername string, mqPassword string, mqEndpoint string) (*amqp.Connection, *amqp.Channel, *amqp.Queue, error) {
	conn, err := amqp.Dial(fmt.Sprintf("amqp://%s:%s@%s/", mqUsername, mqPassword, mqEndpoint))
	if err != nil {
		return nil, nil, nil, err
	}

	// RabbitMQ - Channel
	ch, err := conn.Channel()
	if err != nil {
		return nil, nil, nil, err
	}

	// RabbitMQ - Queue
	queue, err := ch.QueueDeclare(
		"images", // name
		true,     // durable
		false,    // delete when unused
		false,    // exclusive
		false,    // no-wait
		nil,      // arguments
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return conn, ch, &queue, nil
}

func setupObjectStorage(err error, minioEndpoint string, minioAccessKeyID string, minioSecretAccessKey string) (string, *minio.Client, error) {
	// MinIO Make a new bucket called "images".
	bucketName := "images" // TODO make env variable

	// Initialize minio client object.
	minioClient, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioAccessKeyID, minioSecretAccessKey, ""),
		Secure: false,
	})

	if err != nil {
		log.Fatal().Err(err).Msg("could not connect to minio")
	}
	buckets, err := minioClient.ListBuckets(context.Background())
	fmt.Println("Buckets", buckets)

	// Make minIO bucket if not exists
	err = minioClient.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{})
	if err != nil {

		// Check if the bucket already exists (which happens if you run this twice)
		exists, errBucketExists := minioClient.BucketExists(context.Background(), bucketName)

		if errBucketExists == nil && exists {
			log.Printf("MinIO: The bucket %s already existsn", bucketName)
			return bucketName, minioClient, nil
		} else {
			log.Fatal().Err(err).Msg("could not check bucket status")
		}
		return bucketName, minioClient, err
	} else {
		log.Printf("MinIO: Successfully created the bucket %s", bucketName)
	}
	return bucketName, minioClient, nil
}

func setUpDb(postgresHost string, postgresUsername string, postgresPassword string) (*pg.DB, error) {
	db := pg.Connect(&pg.Options{
		Addr:     postgresHost + ":5432", // TODO make env variable
		User:     postgresUsername,
		Password: postgresPassword,
		Database: "irmgard",
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})

	err := createSchema(db)
	if err != nil {
		log.Panic().Err(err).Msg("Failed to set up schema")
	}
	return db, err
}

// Image represents an image.
type Image struct {
	Id              int64  `json:"id"`
	Name            string `json:"name"`
	StorageLocation string `json:"storage_location"`
}

func (i *Image) String() string {
	return fmt.Sprintf("Image<%d %s %s>", i.Id, i.Name, i.StorageLocation)
}

func createSchema(db *pg.DB) error {
	models := []interface{}{
		(*Image)(nil),
	}
	for _, model := range models {
		err := db.Model(model).CreateTable(&orm.CreateTableOptions{IfNotExists: true})
		if err != nil {
			log.Err(err).Msg("Failed to create table")
			return err
		}
	}
	return nil
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatal().Err(err).Msg(msg)
	}
}
