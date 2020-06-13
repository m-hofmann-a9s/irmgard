// webservice
// image upload
// pg dependency
// rabbitmq

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/go-pg/pg"
	"github.com/go-pg/pg/orm"
	"github.com/kataras/iris"
	"github.com/kataras/iris/middleware/logger"
	"github.com/kataras/iris/middleware/recover"
	"github.com/streadway/amqp"
)

func main() {

	// Database
	postgresUsername := os.Getenv("POSTGRES_USERNAME")
	postgresPassword := os.Getenv("POSTGRES_PASSWORD")
	postgresHost := os.Getenv("POSTGRES_HOST")

	db := pg.Connect(&pg.Options{
		Addr:     postgresHost + ":5432", // TODO make env variable
		User:     postgresUsername,
		Password: postgresPassword,
		Database: "postgres", // TODO Make env variable
	})
	defer db.Close()

	err := createSchema(db)
	if err != nil {
		panic(err)
	}

	// RabbitMQ
	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/") // TODO Make username, password, host and port configurable using ENV variables.
	failOnError(err, "Failed to connect to RabbitMQ")
	defer conn.Close()

	// RabbitMQ - Channel
	ch, err := conn.Channel()
	failOnError(err, "Failed to open a channel")
	defer ch.Close()

	// RabbitMQ - Queue
	q, err := ch.QueueDeclare(
		"images", // name
		true,     // durable
		false,    // delete when unused
		false,    // exclusive
		false,    // no-wait
		nil,      // arguments
	)
	failOnError(err, "Failed to declare a queue")

	// Web Service
	app := iris.New()
	app.Logger().SetLevel("debug")

	// Recover panics
	app.Use(recover.New())
	app.Use(logger.New())

	// Method: GET
	// Resource http://localhost:8080
	app.Handle("GET", "/", func(ctx iris.Context) {
		var images []Image
		err := db.Model(&images).Order("id ASC").Select()

		if err != nil {
			panic(err)
		}

		if images == nil {
			images = []Image{}
		}

		ctx.JSON(images)
	})

	app.Handle("POST", "/", func(ctx iris.Context) {
		var image Image
		ctx.ReadJSON(&image)

		// Write to the DB
		err = db.Insert(&image)
		if err != nil {
			ctx.Writef("Error: " + err.Error())
			return
		}

		body, err := json.Marshal(image)
		if (err) != nil {
			panic(err)
		}

		// Write to the message queue
		err = ch.Publish(
			"",     // exchange
			q.Name, // routing key
			false,  // mandatory
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         []byte(body),
			},
		)

		ctx.Writef("Success: %s %s", image.Name, image.StorageLocation)
	})

	app.Run(iris.Addr(":8080"), iris.WithoutServerError(iris.ErrServerClosed))
}

func getIndex(ctx iris.Context) {

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
		err := db.CreateTable(model, &orm.CreateTableOptions{IfNotExists: true})
		if err != nil {
			return err
		}
	}

	return nil
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}
