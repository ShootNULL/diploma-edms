package controllers

import (
	"fmt"
	"time"
)

var ZCBQueue = NewJobQueue(300*time.Millisecond, 5000)
var AIQueue = NewJobQueue(300*time.Millisecond, 5000)
var OCRQueue = NewJobQueue(1000*time.Millisecond, 10)
var GPTQueue = NewJobQueue(1000*time.Millisecond, 10)
var LibreQueue = NewJobQueue(300*time.Millisecond, 10)

func ZCBSendToQueue(f func() []byte) []byte {
	ch := make(chan []byte)
	ZCBQueue.Add(func() error {
		ch <- f()
		return nil
	})
	return <-ch
}

func AISendToQueueAsync(f func() []byte) {
	AIQueue.Add(func() error {
		f()
		return nil
	})
}

func AISendToQueueSync(f func() []byte) []byte {
	ch := make(chan []byte, 1)
	AIQueue.Add(func() error {
		ch <- f()
		return nil
	})
	return <-ch
}

func OCRSendToQueueSync(f func() []byte) []byte {
	ch := make(chan []byte, 1)
	OCRQueue.Add(func() error {
		ch <- f()
		return nil
	})
	return <-ch
}

func GPTSendToQueueSync(f func() string) string {
	ch := make(chan string, 1)
	GPTQueue.Add(func() error {
		ch <- f()
		return nil
	})
	return <-ch
}

func LibreSendToQueueSync(f func() string) string {
	ch := make(chan string, 1)
	LibreQueue.Add(func() error {
		ch <- f()
		return nil
	})
	return <-ch
}

// --------------
func init() {
	ZCBQueue.Run()
	AIQueue.Run()
	OCRQueue.Run()
	GPTQueue.Run()
	LibreQueue.Run()
}

type JobFunc func() error

type JobQueue struct {
	queue       chan JobFunc
	rateLimiter <-chan time.Time
}

func NewJobQueue(rate time.Duration, bufferSize int) *JobQueue {
	return &JobQueue{
		queue:       make(chan JobFunc, bufferSize),
		rateLimiter: time.Tick(rate),
	}
}

func (jq *JobQueue) Add(job JobFunc) {
	jq.queue <- job
}

func (jq *JobQueue) Run() {
	go func() {
		for job := range jq.queue {
			<-jq.rateLimiter
			err := job()
			if err != nil {
				fmt.Println("Ошибка при выполнении job:", err)
			}
		}
	}()
}
