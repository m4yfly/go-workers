package workers

import (
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/garyburd/redigo/redis"
)

var thisIPHash string

func init() {
	// 获取本机的MAC地址，docker内失效
	// interfaces, err := net.Interfaces()
	// if err != nil {
	// 	panic("Error : " + err.Error())
	// }
	// for _, inter := range interfaces {
	// 	if strings.HasPrefix(inter.Name, "en") {
	// 		h2 := md5.New()
	// 		h2.Write([]byte(inter.HardwareAddr.String()))
	// 		thisMac = fmt.Sprintf("%x", h2.Sum(nil))[:16]
	// 		break
	// 	}
	// }
	// fmt.Println(thisMac)
	resp, err := http.Get("http://pv.sohu.com/cityjson")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	content, _ := ioutil.ReadAll(resp.Body)
	h2 := md5.New()
	h2.Write([]byte(content))
	thisIPHash = fmt.Sprintf("%x", h2.Sum(nil))[:16]
}

type Fetcher interface {
	Queue() string
	Fetch()
	Acknowledge(*Msg)
	Ready() chan bool
	FinishedWork() chan bool
	Messages() chan *Msg
	Close()
	Closed() bool
}

type fetch struct {
	queue        string
	ready        chan bool
	finishedwork chan bool
	messages     chan *Msg
	stop         chan bool
	exit         chan bool
	closed       chan bool
}

func NewFetch(queue string, messages chan *Msg, ready chan bool) Fetcher {
	return &fetch{
		queue,
		ready,
		make(chan bool),
		messages,
		make(chan bool),
		make(chan bool),
		make(chan bool),
	}
}

func (f *fetch) Queue() string {
	return f.queue
}

func (f *fetch) processOldMessages() {
	messages := f.inprogressMessages()

	for _, message := range messages {
		<-f.Ready()
		f.sendMessage(message)
	}
}

func (f *fetch) Fetch() {
	f.processOldMessages()

	go func() {
		for {
			// f.Close() has been called
			if f.Closed() {
				break
			}
			<-f.Ready()
			f.tryFetchMessage()
		}
	}()

	for {
		select {
		case <-f.stop:
			// Stop the redis-polling goroutine
			close(f.closed)
			// Signal to Close() that the fetcher has stopped
			close(f.exit)
			break
		}
	}
}

func (f *fetch) tryFetchMessage() {
	conn := Config.Pool.Get()
	defer conn.Close()

	message, err := redis.String(conn.Do("brpoplpush", f.queue, f.inprogressQueue(), 1))

	if err != nil {
		// If redis returns null, the queue is empty. Just ignore the error.
		if err.Error() != "redigo: nil returned" {
			Logger.Println("ERR: ", err)
			time.Sleep(1 * time.Second)
		}
	} else {
		f.sendMessage(message)
	}
}

func (f *fetch) sendMessage(message string) {
	msg, err := NewMsg(message)

	if err != nil {
		Logger.Println("ERR: Couldn't create message from", message, ":", err)
		return
	}

	f.Messages() <- msg
}

func (f *fetch) Acknowledge(message *Msg) {
	conn := Config.Pool.Get()
	defer conn.Close()
	conn.Do("lrem", f.inprogressQueue(), -1, message.OriginalJson())
}

func (f *fetch) Messages() chan *Msg {
	return f.messages
}

func (f *fetch) Ready() chan bool {
	return f.ready
}

func (f *fetch) FinishedWork() chan bool {
	return f.finishedwork
}

func (f *fetch) Close() {
	f.stop <- true
	<-f.exit
}

func (f *fetch) Closed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func (f *fetch) inprogressMessages() []string {
	conn := Config.Pool.Get()
	defer conn.Close()

	messages, err := redis.Strings(conn.Do("lrange", f.inprogressQueue(), 0, -1))
	if err != nil {
		Logger.Println("ERR: ", err)
	}

	return messages
}

func (f *fetch) inprogressQueue() string {
	return fmt.Sprint(f.queue, ":", Config.processId, ":", thisIPHash, ":inprogress")
}
