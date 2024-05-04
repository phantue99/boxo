package rabbitmq

import (
	"encoding/json"
	"log"

	"github.com/streadway/amqp"
)

type RabbitMQ struct {
	conn    *amqp.Connection
	channel *amqp.Channel
	queue   amqp.Queue
}

func NewRabbitMQ(url, queueName string) (*RabbitMQ, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, err
	}

	channel, err := conn.Channel()
	if err != nil {
		return nil, err
	}

	queue, err := channel.QueueDeclare(
		queueName,
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return nil, err
	}

	return &RabbitMQ{
		conn:    conn,
		channel: channel,
		queue:   queue,
	}, nil
}

func (r *RabbitMQ) Publish(payload interface{}) error {
	message, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	err = r.channel.Publish(
		"",
		r.queue.Name,
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        message,
		},
	)
	return err
}

func (r *RabbitMQ) Consume() (<-chan amqp.Delivery, error) {
	msgs, err := r.channel.Consume(
		r.queue.Name,
		"",
		true,
		false,
		false,
		false,
		nil,
	)
	return msgs, err
}

func InitializeRabbitMQ(amqpConnect, queueName string) *RabbitMQ {
	rmq, err := NewRabbitMQ(amqpConnect, queueName)
	if err != nil {
		log.Fatalf("🚀 Could not NewRabbitMQ %s: %v", queueName, err)
	}
	return rmq
}
