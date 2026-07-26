package event

import (
	"cwxu-algo/app/common/conf"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
	"github.com/streadway/amqp"
)

// RabbitMQ 连接封装，支持 Connection 级自动重连。
// amqp.Channel 非并发安全：消费/发布必须使用独立 Channel；
// 发布侧通过 pubMu 串行化，避免多 goroutine 共用一个 Ch。
type RabbitMQ struct {
	// Conn 当前连接（可能短暂为 nil；请用 OpenChannel，勿长期持有）
	Conn *amqp.Connection

	dsn string

	mu          sync.Mutex // 保护 Conn / pubCh
	reconnectMu sync.Mutex // 串行重连，避免并发 Dial 打爆 MQ
	pubMu       sync.Mutex // 串行发布 / declare / purge
	pubCh       *amqp.Channel
	pubConfirm  <-chan amqp.Confirmation
	closed      atomic.Bool
}

func NewRabbitMQ(data *conf.Server) (*RabbitMQ, func(), error) {
	mq := &RabbitMQ{dsn: data.AmqpDsn}
	if err := mq.reconnect(); err != nil {
		return nil, func() {}, err
	}
	return mq, func() {
		mq.closed.Store(true)
		mq.mu.Lock()
		if mq.pubCh != nil {
			_ = mq.pubCh.Close()
			mq.pubCh = nil
		}
		mq.pubConfirm = nil
		if mq.Conn != nil {
			_ = mq.Conn.Close()
			mq.Conn = nil
		}
		mq.mu.Unlock()
	}, nil
}

func (r *RabbitMQ) dial() (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.DialConfig(r.dsn, amqp.Config{
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
	})
	if err != nil {
		return nil, nil, err
	}
	pubCh, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := pubCh.Confirm(false); err != nil {
		_ = pubCh.Close()
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, pubCh, nil
}

// reconnect 建立新连接并替换旧连接；NotifyClose 后后台自动再连。
func (r *RabbitMQ) reconnect() error {
	if r == nil {
		return fmt.Errorf("mq not ready")
	}
	if r.closed.Load() {
		return fmt.Errorf("mq closed")
	}
	r.reconnectMu.Lock()
	defer r.reconnectMu.Unlock()

	// 双重检查：其它路径可能已重连成功
	r.mu.Lock()
	if r.Conn != nil && !r.Conn.IsClosed() {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	conn, pubCh, err := r.dial()
	if err != nil {
		return err
	}

	r.mu.Lock()
	oldConn, oldPub := r.Conn, r.pubCh
	r.Conn, r.pubCh = conn, pubCh
	r.pubConfirm = pubCh.NotifyPublish(make(chan amqp.Confirmation, 1))
	r.mu.Unlock()

	if oldPub != nil {
		_ = oldPub.Close()
	}
	if oldConn != nil {
		_ = oldConn.Close()
	}

	closeCh := make(chan *amqp.Error, 1)
	conn.NotifyClose(closeCh)
	go r.watchConn(conn, closeCh)
	return nil
}

func (r *RabbitMQ) watchConn(conn *amqp.Connection, closeCh chan *amqp.Error) {
	err, ok := <-closeCh
	if r.closed.Load() {
		return
	}
	if ok && err != nil {
		log.Errorf("RabbitMQ connection closed: %v", err)
	} else {
		log.Warnf("RabbitMQ connection closed")
	}

	// 仅当仍是当前连接时清空，避免误清新连接
	r.mu.Lock()
	if r.Conn == conn {
		if r.pubCh != nil {
			_ = r.pubCh.Close()
			r.pubCh = nil
			r.pubConfirm = nil
		}
		r.Conn = nil
	}
	r.mu.Unlock()

	backoff := time.Second
	for !r.closed.Load() {
		// 已被其它路径重连成功则退出
		r.mu.Lock()
		live := r.Conn != nil && !r.Conn.IsClosed()
		r.mu.Unlock()
		if live {
			return
		}
		if reErr := r.reconnect(); reErr == nil {
			log.Infof("RabbitMQ reconnected")
			return
		} else {
			log.Errorf("RabbitMQ reconnect failed: %v，%v 后重试", reErr, backoff)
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// ensureConn 保证有可用连接（必要时同步重连一次）
func (r *RabbitMQ) ensureConn() error {
	if r == nil {
		return fmt.Errorf("mq not ready")
	}
	if r.closed.Load() {
		return fmt.Errorf("mq closed")
	}
	r.mu.Lock()
	live := r.Conn != nil && !r.Conn.IsClosed()
	r.mu.Unlock()
	if live {
		return nil
	}
	return r.reconnect()
}

func (r *RabbitMQ) invalidate(conn *amqp.Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if conn != nil && r.Conn != conn {
		return
	}
	if r.pubCh != nil {
		_ = r.pubCh.Close()
		r.pubCh = nil
		r.pubConfirm = nil
	}
	if r.Conn != nil {
		_ = r.Conn.Close()
		r.Conn = nil
	}
}

// OpenChannel 打开新 Channel（供消费者专用，勿与 Publish 共用）
func (r *RabbitMQ) OpenChannel() (*amqp.Channel, error) {
	if err := r.ensureConn(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	conn := r.Conn
	r.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("mq not ready")
	}
	ch, err := conn.Channel()
	if err != nil {
		r.invalidate(conn)
		// 再试一次重连
		if err2 := r.ensureConn(); err2 != nil {
			return nil, err
		}
		r.mu.Lock()
		conn = r.Conn
		r.mu.Unlock()
		if conn == nil {
			return nil, err
		}
		return conn.Channel()
	}
	return ch, nil
}

// ensurePubCh 确保发布 channel 可用（调用方须持有 pubMu）
func (r *RabbitMQ) ensurePubCh() error {
	if err := r.ensureConn(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return fmt.Errorf("mq closed")
	}
	if r.Conn == nil || r.Conn.IsClosed() {
		return fmt.Errorf("mq not ready")
	}
	if r.pubCh != nil {
		return nil
	}
	ch, err := r.Conn.Channel()
	if err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return err
	}
	r.pubCh = ch
	r.pubConfirm = ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	return nil
}

func (r *RabbitMQ) dropPubCh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pubCh != nil {
		_ = r.pubCh.Close()
		r.pubCh = nil
		r.pubConfirm = nil
	}
}

// pubConfirmWait 同步 Publish 等 confirm 的上限。
// 交互 HTTP 路径严禁长时间阻塞：5s×2 次重试 ≈ 网关 10s 504。
const pubConfirmWait = 1500 * time.Millisecond

// Publish 线程安全发布；失败时丢弃 pub channel 并尝试重连再发一次。
// confirm 超时缩短为 1.5s，避免 MQ 半开时拖死 HTTP。
func (r *RabbitMQ) Publish(exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error {
	if r == nil {
		return fmt.Errorf("mq not ready")
	}
	r.pubMu.Lock()
	defer r.pubMu.Unlock()

	try := func() error {
		if err := r.ensurePubCh(); err != nil {
			return err
		}
		r.mu.Lock()
		ch := r.pubCh
		confirm := r.pubConfirm
		r.mu.Unlock()
		if ch == nil || confirm == nil {
			return fmt.Errorf("mq not ready")
		}
		if err := ch.Publish(exchange, key, mandatory, immediate, msg); err != nil {
			return err
		}
		select {
		case confirmation, ok := <-confirm:
			if !ok {
				// confirm 通道被关：channel 已失效，直接丢弃
				r.dropPubCh()
				return fmt.Errorf("mq publisher confirm rejected")
			}
			if !confirmation.Ack {
				return fmt.Errorf("mq publisher confirm rejected")
			}
			return nil
		case <-time.After(pubConfirmWait):
			// 超时：迟到的 confirm 会滞留在该 channel，若继续复用会导致
			// 下一次发布读到过期 confirm 而确认错位——所有超时路径必须丢弃 pub channel
			r.dropPubCh()
			return fmt.Errorf("mq publisher confirm timeout")
		}
	}

	if err := try(); err != nil {
		r.dropPubCh()
		_ = r.reconnect()
		return try()
	}
	return nil
}

// PublishAsync 后台投递：HTTP 路径请用此方法，绝不阻塞调用方。
// 失败只打日志；业务侧应另有直爬/重试兜底。
func (r *RabbitMQ) PublishAsync(exchange, key string, mandatory, immediate bool, msg amqp.Publishing) {
	if r == nil {
		return
	}
	// 拷贝 body，避免调用方复用底层缓冲
	body := append([]byte(nil), msg.Body...)
	msg.Body = body
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Errorf("PublishAsync panic: %v", rec)
			}
		}()
		if err := r.Publish(exchange, key, mandatory, immediate, msg); err != nil {
			log.Warnf("PublishAsync %s: %v", key, err)
		}
	}()
}

// QueueDeclare 线程安全声明队列（发布前）
func (r *RabbitMQ) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	if r == nil {
		return amqp.Queue{}, fmt.Errorf("mq not ready")
	}
	r.pubMu.Lock()
	defer r.pubMu.Unlock()

	try := func() (amqp.Queue, error) {
		if err := r.ensurePubCh(); err != nil {
			return amqp.Queue{}, err
		}
		r.mu.Lock()
		ch := r.pubCh
		r.mu.Unlock()
		if ch == nil {
			return amqp.Queue{}, fmt.Errorf("mq not ready")
		}
		return ch.QueueDeclare(name, durable, autoDelete, exclusive, noWait, args)
	}

	q, err := try()
	if err != nil {
		r.dropPubCh()
		_ = r.reconnect()
		return try()
	}
	return q, nil
}

// QueuePurge 线程安全清空队列
func (r *RabbitMQ) QueuePurge(name string, noWait bool) (int, error) {
	if r == nil {
		return 0, fmt.Errorf("mq not ready")
	}
	r.pubMu.Lock()
	defer r.pubMu.Unlock()

	try := func() (int, error) {
		if err := r.ensurePubCh(); err != nil {
			return 0, err
		}
		r.mu.Lock()
		ch := r.pubCh
		r.mu.Unlock()
		if ch == nil {
			return 0, fmt.Errorf("mq not ready")
		}
		return ch.QueuePurge(name, noWait)
	}

	n, err := try()
	if err != nil {
		r.dropPubCh()
		_ = r.reconnect()
		return try()
	}
	return n, nil
}

// QueueInspect 用临时 channel 被动查询队列（消息数/消费者数），不干扰消费
func (r *RabbitMQ) QueueInspect(name string) (amqp.Queue, error) {
	ch, err := r.OpenChannel()
	if err != nil {
		return amqp.Queue{}, err
	}
	defer ch.Close()
	return ch.QueueInspect(name)
}

// Ch 兼容旧代码：返回发布 channel（已不推荐直接使用）
func (r *RabbitMQ) Ch() *amqp.Channel {
	if r == nil {
		return nil
	}
	r.pubMu.Lock()
	defer r.pubMu.Unlock()
	_ = r.ensurePubCh()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pubCh
}

var ProviderSet = wire.NewSet(NewRabbitMQ)
