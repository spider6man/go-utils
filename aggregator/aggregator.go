package aggregator

import (
	"runtime"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type Aggregator struct {
	option         AggregatorOption
	wg             *sync.WaitGroup
	quit           chan struct{}
	eventQueue     chan interface{}
	batchProcessor BatchProcessFunc
}

type AggregatorOption struct {
	BatchSize         int
	Workers           int
	ChannelBufferSize int
	LingerTime        time.Duration
	ErrorHandler      ErrorHandlerFunc
	Logger            logrus.FieldLogger
}

type BatchProcessFunc func([]interface{}) error

type ErrorHandlerFunc func(err error, items []interface{}, batchProcessFunc BatchProcessFunc, aggregator *Aggregator)

func NewAggregator(batchProcessor BatchProcessFunc, option *AggregatorOption) *Aggregator {
	if option == nil {
		option = &AggregatorOption{
			BatchSize:  8,
			Workers:    runtime.NumCPU(),
			LingerTime: 1 * time.Minute,
		}
	}

	if option.ChannelBufferSize <= option.Workers {
		option.ChannelBufferSize = option.Workers
	}

	return &Aggregator{
		eventQueue:     make(chan interface{}, option.ChannelBufferSize),
		option:         *option,
		quit:           make(chan struct{}),
		wg:             new(sync.WaitGroup),
		batchProcessor: batchProcessor,
	}
}

// Try enqueue an item, and it is non-blocked
func (agt *Aggregator) TryEnqueue(item interface{}) bool {
	select {
	case agt.eventQueue <- item:
		return true
	default:
		if agt.option.Logger != nil {
			agt.option.Logger.Warnf("Aggregator: Event queue is full and try reschedule")
		}
		runtime.Gosched()

		select {
		case agt.eventQueue <- item:
			return true
		default:
			if agt.option.Logger != nil {
				agt.option.Logger.Warnf("Aggregator: Event queue is still full and %+v is skipped.", item)
			}
			return false
		}
	}
}

// Enqueue an item, will be blocked if the queue is full
func (agt *Aggregator) Enqueue(item interface{}) {
	agt.eventQueue <- item
}

// Start the aggregator
func (agt *Aggregator) Start() {
	for i := 0; i < agt.option.Workers; i++ {
		index := i
		go agt.work(index)
	}
}

// Stop the aggregator
func (agt *Aggregator) Stop() {
	close(agt.quit)
	agt.wg.Wait()
}

// Stop the aggregator safely, the difference with Stop is it guarantees no item is missed during stop
func (agt *Aggregator) SafeStop() {
	if len(agt.eventQueue) == 0 {
		close(agt.quit)
	} else {
		ticker := time.NewTicker(50 * time.Millisecond)
		for range ticker.C {
			if len(agt.eventQueue) == 0 {
				close(agt.quit)
				break
			}
		}
		ticker.Stop()
	}
	agt.wg.Wait()
}

func (agt *Aggregator) work(index int) {
	defer func() {
		if r := recover(); r != nil {
			if agt.option.Logger != nil {
				agt.option.Logger.Errorf("Aggregator: recover worker as bad thing happens %+v", r)
			}

			agt.work(index)
		}
	}()

	agt.wg.Add(1)
	defer agt.wg.Done()

	batch := make([]interface{}, 0, agt.option.BatchSize)
	lingerTimer := time.NewTimer(0)
	if !lingerTimer.Stop() {
		<-lingerTimer.C
	}
	defer lingerTimer.Stop()

loop:
	for {
		select {
		case req := <-agt.eventQueue:
			batch = append(batch, req)

			batchSize := len(batch)
			if batchSize < agt.option.BatchSize {
				if batchSize == 1 {
					lingerTimer.Reset(agt.option.LingerTime)
				}
				break
			}

			agt.batchProcess(batch)

			if !lingerTimer.Stop() {
				<-lingerTimer.C
			}
			batch = make([]interface{}, 0, agt.option.BatchSize)
		case <-lingerTimer.C:
			if len(batch) == 0 {
				break
			}

			agt.batchProcess(batch)
			batch = make([]interface{}, 0, agt.option.BatchSize)
		case <-agt.quit:
			if len(batch) != 0 {
				agt.batchProcess(batch)
			}

			break loop
		}
	}
}

func (agt *Aggregator) batchProcess(items []interface{}) {
	agt.wg.Add(1)
	defer agt.wg.Done()
	if err := agt.batchProcessor(items); err != nil {
		if agt.option.Logger != nil {
			agt.option.Logger.Errorf("Aggregator: error happens")
		}

		if agt.option.ErrorHandler != nil {
			go agt.option.ErrorHandler(err, items, agt.batchProcessor, agt)
		} else if agt.option.Logger != nil {
			agt.option.Logger.Errorf("Aggregator: error happens in batchProcess and is skipped")
		}
	} else if agt.option.Logger != nil {
		agt.option.Logger.Debugf("Aggregator: %d items have been sent.", len(items))
	}
}
