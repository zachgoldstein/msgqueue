package ironmq

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-msgqueue/msgqueue"
	"github.com/go-msgqueue/msgqueue/internal"
	"github.com/go-msgqueue/msgqueue/memqueue"
	"github.com/go-msgqueue/msgqueue/processor"

	"github.com/iron-io/iron_go3/api"
	"github.com/iron-io/iron_go3/mq"
)

type Queue struct {
	q        mq.Queue
	opt      *msgqueue.Options
	memqueue *memqueue.Queue

	p *processor.Processor
}

var _ processor.Queuer = (*Queue)(nil)

func NewQueue(mqueue mq.Queue, opt *msgqueue.Options) *Queue {
	if opt.Name == "" {
		opt.Name = mqueue.Name
	}
	opt.Init()

	q := Queue{
		q:   mqueue,
		opt: opt,
	}

	memopt := msgqueue.Options{
		Name:      opt.Name,
		GroupName: opt.GroupName,

		RetryLimit: 3,
		MinBackoff: time.Second,
		Handler:    msgqueue.HandlerFunc(q.add),

		Redis: opt.Redis,
	}
	if opt.Handler != nil {
		memopt.FallbackHandler = internal.MessageUnwrapperHandler(opt.Handler)
	}
	q.memqueue = memqueue.NewQueue(&memopt)

	registerQueue(&q)
	return &q
}

func (q *Queue) Name() string {
	return q.q.Name
}

func (q *Queue) String() string {
	return fmt.Sprintf("Queue<Name=%s>", q.Name())
}

func (q *Queue) Options() *msgqueue.Options {
	return q.opt
}

func (q *Queue) Processor() *processor.Processor {
	if q.p == nil {
		q.p = processor.New(q, q.opt)
	}
	return q.p
}

func (q *Queue) createQueue() error {
	_, err := mq.ConfigCreateQueue(mq.QueueInfo{Name: q.q.Name}, &q.q.Settings)
	return err
}

func (q *Queue) add(msg *msgqueue.Message) error {
	msg = msg.Args[0].(*msgqueue.Message)

	body, err := msg.MarshalArgs()
	if err != nil {
		return err
	}

	id, err := q.q.PushMessage(mq.Message{
		Body:  body,
		Delay: int64(msg.Delay / time.Second),
	})
	if err != nil {
		return err
	}

	msg.Id = id
	return nil
}

// Add adds message to the queue.
func (q *Queue) Add(msg *msgqueue.Message) error {
	return q.memqueue.Add(internal.WrapMessage(msg))
}

// Call creates a message using the args and adds it to the queue.
func (q *Queue) Call(args ...interface{}) error {
	msg := msgqueue.NewMessage(args...)
	return q.Add(msg)
}

// CallOnce works like Call, but it adds message with same args
// only once in a period.
func (q *Queue) CallOnce(period time.Duration, args ...interface{}) error {
	msg := msgqueue.NewMessage(args...)
	msg.SetDelayName(period, args...)
	return q.Add(msg)
}

func (q *Queue) ReserveN(n int) ([]msgqueue.Message, error) {
	if n > 100 {
		n = 100
	}
	mqMsgs, err := q.q.LongPoll(n, int(q.opt.ReservationTimeout/time.Second), 1, false)
	if err != nil {
		if v, ok := err.(api.HTTPResponseError); ok && v.StatusCode() == 404 {
			if strings.Contains(v.Error(), "Message not found") {
				return nil, nil
			}
			if strings.Contains(v.Error(), "Queue not found") {
				_ = q.createQueue()
			}
		}
		return nil, err
	}

	msgs := make([]msgqueue.Message, len(mqMsgs))
	for i, mqMsg := range mqMsgs {
		msgs[i] = msgqueue.Message{
			Id:   mqMsg.Id,
			Body: mqMsg.Body,

			ReservationId: mqMsg.ReservationId,
			ReservedCount: mqMsg.ReservedCount,
		}
	}
	return msgs, nil
}

func (q *Queue) Release(msg *msgqueue.Message, delay time.Duration) error {
	return retry(func() error {
		return q.q.ReleaseMessage(msg.Id, msg.ReservationId, int64(delay/time.Second))
	})
}

func (q *Queue) Delete(msg *msgqueue.Message) error {
	err := retry(func() error {
		return q.q.DeleteMessage(msg.Id, msg.ReservationId)
	})
	if err == nil {
		return nil
	}
	if v, ok := err.(api.HTTPResponseError); ok && v.StatusCode() == 404 {
		return nil
	}
	return err
}

func (q *Queue) DeleteBatch(msgs []*msgqueue.Message) error {
	mqMsgs := make([]mq.Message, len(msgs))
	for i, msg := range msgs {
		mqMsgs[i] = mq.Message{
			Id:            msg.Id,
			ReservationId: msg.ReservationId,
		}
	}
	return retry(func() error {
		return q.q.DeleteReservedMessages(mqMsgs)
	})
}

func (q *Queue) Purge() error {
	return q.q.Clear()
}

// Close is CloseTimeout with 30 seconds timeout.
func (q *Queue) Close() error {
	return q.CloseTimeout(30 * time.Second)
}

// Close closes the queue waiting for pending messages to be processed.
func (q *Queue) CloseTimeout(timeout time.Duration) error {
	var firstErr error
	if err := q.memqueue.CloseTimeout(timeout); err != nil && firstErr == nil {
		firstErr = err
	}
	if q.p != nil {
		if err := q.p.StopTimeout(timeout); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func retry(fn func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		err = fn()
		if err == nil {
			return nil
		}
		if v, ok := err.(api.HTTPResponseError); ok && v.StatusCode() >= 500 {
			continue
		}
		break
	}
	return err
}
