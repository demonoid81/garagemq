package vhost

import (
	"errors"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/valinurovam/garagemq/amqp"
	"github.com/valinurovam/garagemq/binding"
	"github.com/valinurovam/garagemq/config"
	"github.com/valinurovam/garagemq/exchange"
	"github.com/valinurovam/garagemq/interfaces"
	"github.com/valinurovam/garagemq/msgstorage"
	"github.com/valinurovam/garagemq/queue"
)

const EX_DEFAULT_NAME = ""

type VirtualHost struct {
	name       string
	system     bool
	exLock     sync.Mutex
	exchanges  map[string]*exchange.Exchange
	quLock     sync.Mutex
	queues     map[string]interfaces.AmqpQueue
	msgStorage *msgstorage.MsgStorage
	srvStorage interfaces.DbStorage
	srvConfig  *config.Config
	logger     *log.Entry
}

func New(name string, system bool, msgStorage *msgstorage.MsgStorage, srvStorage interfaces.DbStorage, srvConfig *config.Config) *VirtualHost {
	vhost := &VirtualHost{
		name:       name,
		system:     system,
		exchanges:  make(map[string]*exchange.Exchange),
		queues:     make(map[string]interfaces.AmqpQueue),
		msgStorage: msgStorage,
		srvStorage: srvStorage,
		srvConfig:  srvConfig,
	}

	vhost.logger = log.WithFields(log.Fields{
		"vhost": name,
	})

	vhost.initSystemExchanges()
	vhost.loadQueues()

	vhost.logger.Info("Load messages into queues")
	vhost.msgStorage.LoadIntoQueues(vhost.queues)
	for _, q := range vhost.GetQueues() {
		q.Start()
		vhost.logger.WithFields(log.Fields{
			"name":   q.GetName(),
			"length": q.Length(),
		}).Info("Messages loaded into queue")
	}

	return vhost
}

func (vhost *VirtualHost) initSystemExchanges() {
	vhost.logger.Info("Initialize host default exchanges...")
	for _, exType := range []int{
		exchange.EX_TYPE_DIRECT,
		exchange.EX_TYPE_FANOUT,
		exchange.EX_TYPE_HEADERS,
		exchange.EX_TYPE_TOPIC,
	} {
		exTypeAlias, _ := exchange.GetExchangeTypeAlias(exType)
		exName := "amq." + exTypeAlias
		vhost.AppendExchange(exchange.New(exName, exType, true, false, false, true, &amqp.Table{}))
	}

	systemExchange := exchange.New(EX_DEFAULT_NAME, exchange.EX_TYPE_DIRECT, true, false, false, true, &amqp.Table{})
	vhost.AppendExchange(systemExchange)
}

func (vhost *VirtualHost) GetQueue(name string) interfaces.AmqpQueue {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()
	return vhost.getQueue(name)
}

func (vhost *VirtualHost) GetQueues() map[string]interfaces.AmqpQueue {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()
	return vhost.queues
}

func (vhost *VirtualHost) getQueue(name string) interfaces.AmqpQueue {
	return vhost.queues[name]
}

func (vhost *VirtualHost) GetExchange(name string) *exchange.Exchange {
	vhost.exLock.Lock()
	defer vhost.exLock.Unlock()
	return vhost.getExchange(name)
}

func (vhost *VirtualHost) getExchange(name string) *exchange.Exchange {
	return vhost.exchanges[name]
}

func (vhost *VirtualHost) GetDefaultExchange() *exchange.Exchange {
	return vhost.exchanges[EX_DEFAULT_NAME]
}

func (vhost *VirtualHost) AppendExchange(ex *exchange.Exchange) {
	vhost.exLock.Lock()
	defer vhost.exLock.Unlock()
	exTypeAlias, _ := exchange.GetExchangeTypeAlias(ex.ExType)
	vhost.logger.WithFields(log.Fields{
		"name": ex.Name,
		"type": exTypeAlias,
	}).Info("Append exchange")
	vhost.exchanges[ex.Name] = ex
}

func (vhost *VirtualHost) NewQueue(name string, connId uint64, exclusive bool, autoDelete bool, durable bool, shardSize int) interfaces.AmqpQueue {
	return queue.NewQueue(
		name,
		connId,
		exclusive,
		autoDelete,
		durable,
		shardSize,
		vhost.msgStorage,
	)
}

func (vhost *VirtualHost) AppendQueue(qu interfaces.AmqpQueue) {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()
	vhost.logger.WithFields(log.Fields{
		"queueName": qu.GetName(),
	}).Info("Append queue")

	vhost.queues[qu.GetName()] = qu

	ex := vhost.GetDefaultExchange()
	bind := binding.New(qu.GetName(), EX_DEFAULT_NAME, qu.GetName(), &amqp.Table{}, false)
	ex.AppendBinding(bind)

	vhost.saveQueues()
}

func (vhost *VirtualHost) getKeyName() string {
	if vhost.name == "/" {
		return "default"
	} else {
		return vhost.name
	}
}
func (vhost *VirtualHost) saveQueues() {
	var queueNames []string
	for name, q := range vhost.queues {
		if !q.IsDurable() {
			continue
		}
		queueNames = append(queueNames, name)
	}
	vhost.srvStorage.Set(vhost.getKeyName()+".queues", []byte(strings.Join(queueNames, "\n")))
}

func (vhost *VirtualHost) loadQueues() {
	// TODO incapsulate into server
	vhost.logger.Info("Initialize queues...")
	queues, err := vhost.srvStorage.Get(vhost.getKeyName() + ".queues")
	if err != nil || len(queues) == 0 {
		return
	}
	queueNames := strings.Split(string(queues), "\n")
	for _, name := range queueNames {
		vhost.AppendQueue(
			vhost.NewQueue(name, 0, false, false, true, vhost.srvConfig.Queue.ShardSize),
		)
	}
}

func (vhost *VirtualHost) DeleteQueue(queueName string, ifUnused bool, ifEmpty bool) (uint64, error) {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()

	qu := vhost.getQueue(queueName)
	if qu == nil {
		return 0, errors.New("not found")
	}

	var length, err = qu.Delete(ifUnused, ifEmpty)
	if err != nil {
		return 0, err
	}
	for _, ex := range vhost.exchanges {
		ex.RemoveQueueBindings(queueName)
	}
	delete(vhost.queues, queueName)

	return length, nil
}

func (vhost *VirtualHost) Stop() error {
	vhost.quLock.Lock()
	vhost.exLock.Lock()
	defer vhost.quLock.Unlock()
	defer vhost.exLock.Unlock()
	vhost.logger.Info("Stop virtual host")
	for _, qu := range vhost.queues {
		qu.Stop()
		vhost.logger.WithFields(log.Fields{
			"queueName": qu.GetName(),
		}).Info("Queue stopped")
	}

	vhost.msgStorage.Close()
	vhost.logger.Info("Storage closed")
	return nil
}

func (vhost *VirtualHost) Name() string {
	return vhost.name
}
