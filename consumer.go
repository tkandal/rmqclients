package amqclient

import (
	"context"
	"crypto/tls"
	"github.com/streadway/amqp"
	"go.uber.org/zap"
	"net"
	"time"
)

type Consumer struct {
	amqpURI        string
	tls            *tls.Config
	exchange       string
	exchangeType   string
	queue          string
	routingKey     string
	ctag           string
	logger         *zap.SugaredLogger
	client         *client
	clientChanChan chan chan *client
	sendChan       chan amqp.Delivery
	cancel         context.CancelFunc
	quit           chan struct{}
}

func NewConsumer(amqpURI string, tls *tls.Config, exchange string, exchangeType string, queue string, key string,
	ctag string, logger *zap.SugaredLogger) (*Consumer, chan amqp.Delivery, error) {

	c := &Consumer{
		amqpURI:      amqpURI,
		tls:          tls,
		exchange:     exchange,
		exchangeType: exchangeType,
		queue:        queue,
		routingKey:   key,
		ctag:         ctag,
		logger:       logger,
		sendChan:     make(chan amqp.Delivery),
		quit:         make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.clientChanChan = redialConsumer(ctx, c)

	go c.handle()
	return c, c.sendChan, nil
}

func (c *Consumer) Shutdown() {
	c.logger.Warn("consumer received shutdown ...")
	close(c.quit)
	c.cancel()
}

func (c *Consumer) handle() {

	var deliveries <-chan amqp.Delivery
	var err error
	for {
		if c.client == nil {
			clientChan, ok := <-c.clientChanChan
			if !ok {
				c.logger.Errorf("cannot get a new client; channel is closed")
				return
			}
			c.client = <-clientChan
			c.logger.Debugf("queue bound to exchange, starting consume (consumer tag '%s')", c.ctag)
			deliveries, err = c.client.channel.Consume(
				c.queue, // name
				c.ctag,  // consumerTag,
				true,    // autoAck
				false,   // exclusive
				false,   // noLocal
				false,   // noWait
				nil,     // arguments
			)
			if err != nil {
				c.logger.Errorf("deliver channel failed; error = %v", err)
				_ = c.client.close()
				c.client = nil
				continue
			}
		}

		select {
		case d, ok := <-deliveries:
			if !ok {
				c.logger.Error("deliveries channel closed")
				_ = c.client.close()
				c.client = nil
				continue
			}
			if err := d.Ack(false); err != nil {
				_ = c.client.close()
				c.client = nil
				continue
			}
			c.sendChan <- d

		case <-c.quit:
			close(c.sendChan)
			return
		}
	}
}

func redialConsumer(ctx context.Context, con *Consumer) chan chan *client {
	clientChanChan := make(chan chan *client)

	go func() {
		clientChan := make(chan *client)
		defer close(clientChanChan)
		defer close(clientChan)

		for {
			select {
			case clientChanChan <- clientChan:
			case <-ctx.Done():
				con.logger.Errorf("context done; error = %v", ctx.Done())
				return
			}

			var err error
			c := &client{
				connection: nil,
				channel:    nil,
				confirms:   nil,
			}
			con.logger.Debugf("Connecting to %s", con.amqpURI)
			cfg := amqp.Config{
				Heartbeat: 10 * time.Second,
				Dial: func(nw string, addr string) (net.Conn, error) {
					return net.DialTimeout(nw, addr, 10*time.Second)
				},
			}
			if con.tls != nil {
				cfg.TLSClientConfig = con.tls
			}

			c.connection, err = amqp.DialConfig(con.amqpURI, cfg)
			if err != nil {
				con.logger.Errorf("dial %s failed; error = %v ", con.amqpURI, err)
				return
			}

			con.logger.Debug("Getting Channel")
			c.channel, err = c.connection.Channel()
			if err != nil {
				con.logger.Errorf("get channel failed; error = %v", err)
				return
			}

			con.logger.Debugf("Declaring Exchange (%s)", con.exchange)
			if err = c.channel.ExchangeDeclare(
				con.exchange,     // name of the exchange
				con.exchangeType, // type
				true,             // durable
				false,            // delete when complete
				false,            // internal
				false,            // noWait
				nil,              // arguments
			); err != nil {
				con.logger.Errorf("declare exchange failed; error = %v", err)
				return
			}

			con.logger.Debugf("Declaring Queue (%s)", con.queue)
			state, err := c.channel.QueueDeclare(
				con.queue, // name of the queue
				true,      // durable
				false,     // delete when usused
				false,     // exclusive
				false,     // noWait
				nil,       // arguments
			)
			if err != nil {
				con.logger.Errorf("declare queue failed; error = %v", err)
				return
			}

			con.logger.Debugf("Declared Queue (%d messages, %d consumers), binding to Exchange (key '%s')",
				state.Messages, state.Consumers, con.routingKey)
			if err = c.channel.QueueBind(
				con.queue,      // name of the queue
				con.routingKey, // routingKey
				con.exchange,   // sourceExchange
				false,          // noWait
				nil,            // arguments
			); err != nil {
				con.logger.Errorf("bind queue failed; error = %v", err)
				return
			}

			select {
			case clientChan <- c:
			case <-ctx.Done():
				con.logger.Errorf("context done; error = %v", ctx.Err())
				return
			}
		}
	}()

	return clientChanChan
}
