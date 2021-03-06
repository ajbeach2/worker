package sqsworker

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/aws/aws-sdk-go/service/sns/snsiface"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sqs/sqsiface"
	"go.uber.org/zap"
	"os"
	"runtime"
	"sync"
)

// DefaultWorkers Number of worker goroutines to spawn, each runs the handler function
const DefaultWorkers = 1

// DefaultMaxNumberOfMessages amount of messages received by each SQS request
const DefaultMaxNumberOfMessages = 10

// DefaultVisibilityTimeout SQS visibility Timeout
const DefaultVisibilityTimeout = 60

// DefaultWaitTimeSeconds Long-polling interval for SQS
const DefaultWaitTimeSeconds = 20

// Handler interface for SQS consumers
type Processor interface {
	Process(context.Context, *sqs.Message, *sns.PublishInput) error
}

// Callback which is passed result from handler on success
type Callback func(*string, error)

// Worker encapsulates the SQS consumer
type Worker struct {
	QueueURL  string
	TopicArn  string
	Queue     sqsiface.SQSAPI
	Topic     snsiface.SNSAPI
	Session   *session.Session
	Consumers int
	Logger    *zap.Logger
	Processor Processor
	Callback  Callback
	Name      string
	done      chan error
}

// WorkerConfig settings for Worker to be passed in NewWorker Contstuctor
type WorkerConfig struct {
	QueueURL string
	TopicArn string
	// If the number of workers is 0, the number of workers defaults to runtime.NumCPU()
	Workers   int
	Processor Processor
	Callback  Callback
	Name      string
	Logger    *zap.Logger
}

func (w *Worker) logError(msg string, err error) {
	if w.Logger != nil {
		w.Logger.Error(err.Error(),
			zap.String("app", w.Name),
			zap.String("msg", msg),
			zap.Error(err),
		)
	}
}

func (w *Worker) logInfo(msg string) {
	if w.Logger != nil {
		w.Logger.Info(msg,
			zap.String("app", w.Name),
		)
	}
}

func (w *Worker) deleteMessage(m *sqs.DeleteMessageInput) error {
	_, err := w.Queue.DeleteMessage(m)
	if err != nil {
		return err
	}
	return nil
}

func (w *Worker) sendMessage(msg *sns.PublishInput) error {
	if w.TopicArn == "" {
		return nil
	}

	if msg.Message == nil {
		return nil
	}

	_, err := w.Topic.Publish(msg)
	return err
}

func (w *Worker) consumer(ctx context.Context, in chan *sqs.Message) {
	var msgString string
	deleteInput := &sqs.DeleteMessageInput{QueueUrl: &w.QueueURL}
	var sendInput *sns.PublishInput
	var err error
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-in:
			if w.Callback != nil || w.TopicArn != "" {
				sendInput = &sns.PublishInput{TopicArn: &w.TopicArn, Message: &msgString}
			}
			err = w.Processor.Process(ctx, msg, sendInput)
			if err == nil {
				err = w.sendMessage(sendInput)
				if err != nil {
					w.logError("send message failed!", err)
				}
				deleteInput.ReceiptHandle = msg.ReceiptHandle
				err = w.deleteMessage(deleteInput)
				if err != nil {
					w.logError("delete message failed!", err)
				}
			} else {
				w.logError("handler failed!", err)
			}

			if w.Callback != nil {
				w.Callback(sendInput.Message, err)
			}
		}
	}
}

func (w *Worker) producer(ctx context.Context, out chan *sqs.Message) {
	params := &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(w.QueueURL),
		MaxNumberOfMessages: aws.Int64(DefaultMaxNumberOfMessages),
		VisibilityTimeout:   aws.Int64(DefaultVisibilityTimeout),
		WaitTimeSeconds:     aws.Int64(DefaultWaitTimeSeconds),
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			req, resp := w.Queue.ReceiveMessageRequest(params)
			err := req.Send()
			if err != nil {
				w.logError("receive messages failed!", err)
			} else {
				messages := resp.Messages
				if len(messages) > 0 {
					for _, message := range messages {
						out <- message
					}
				}
			}
		}
	}
}

// Close function will send a signal to all workers to exit
func (w *Worker) Close() {
	close(w.done)
}

// Run does the main consumer/producer loop
func (w *Worker) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	messages := make(chan *sqs.Message, w.Consumers)

	w.logInfo(fmt.Sprint("Staring producer"))
	go func() {
		w.producer(ctx, messages)
		close(messages)
	}()

	go func() {
		<-w.done
		cancel()
	}()

	w.logInfo(fmt.Sprint("Staring consumer with ", w.Consumers, " consumers"))
	// Consume messages
	var wg sync.WaitGroup
	for x := 0; x < w.Consumers; x++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.consumer(ctx, messages)
		}()
	}
	wg.Wait()
}

// CreateQueue Create queue by name.
func CreateQueue(name string, sqsc sqsiface.SQSAPI) (string, error) {
	result, err := sqsc.CreateQueue(&sqs.CreateQueueInput{
		QueueName: aws.String(name),
	})
	if err != nil {
		return "", err
	}

	return *result.QueueUrl, nil
}

// GetOrCreateQueue an SQS Queue by name.
func GetOrCreateQueue(name string, sqsc sqsiface.SQSAPI) (string, error) {
	queueOut, err := sqsc.GetQueueUrl(&sqs.GetQueueUrlInput{
		QueueName: aws.String(name),
	})

	if aerr, ok := err.(awserr.Error); ok {
		switch aerr.Code() {
		case sqs.ErrCodeQueueDoesNotExist:
			return CreateQueue(name, sqsc)
		}
	}

	return *queueOut.QueueUrl, err
}

// GetOrCreateTopic Create SNS topic by name.
func GetOrCreateTopic(name string, snsc snsiface.SNSAPI) (string, error) {
	if name == "" {
		return "", nil
	}

	snsOut, err := snsc.CreateTopic(&sns.CreateTopicInput{
		Name: aws.String(name),
	})

	return *snsOut.TopicArn, err
}

// NewWorker constructor for SQS Worker
func NewWorker(sess *session.Session, wc WorkerConfig) *Worker {
	var logger *zap.Logger
	workers := runtime.NumCPU()
	var queueURL, topicARN = wc.QueueURL, wc.TopicArn

	if wc.Workers != 0 {
		workers = wc.Workers
	}

	if wc.Logger == nil {
		logger, _ = zap.NewProduction()
	} else {
		logger = wc.Logger
	}

	if queueURL == "" {
		queueURL = os.Getenv("QUEUE_URL")
	}

	if topicARN == "" {
		topicARN = os.Getenv("TOPIC_ARN")
	}

	return &Worker{
		queueURL,
		topicARN,
		sqs.New(sess),
		sns.New(sess),
		sess,
		workers,
		logger,
		wc.Processor,
		wc.Callback,
		wc.Name,
		make(chan error),
	}
}
