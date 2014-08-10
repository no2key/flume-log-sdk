package consumer

import (
	"flume-log-sdk/config"
	"flume-log-sdk/consumer/pool"
	"fmt"
	"github.com/blackbeans/redigo/redis"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"
)

type SinkManager struct {
	zkmanager *config.ZKManager

	sinkServers map[string]*SinkServer //业务名称和sinkserver对应

	hp2flumeClientPool map[config.HostPort]*pool.FlumePoolLink //对应的Pool

	redisPool map[string][]*redis.Pool // 对应的redispool

	watcherPool map[string]*config.Watcher //watcherPool

	mutex sync.Mutex

	isRunning bool
}

func NewSinkManager(option *config.Option) *SinkManager {

	sinkmanager := &SinkManager{}
	sinkmanager.sinkServers = make(map[string]*SinkServer)
	sinkmanager.hp2flumeClientPool = make(map[config.HostPort]*pool.FlumePoolLink)
	sinkmanager.watcherPool = make(map[string]*config.Watcher)
	sinkmanager.redisPool = initRedisQueue(option)
	//从zk中拉取flumenode的配置
	zkmanager := config.NewZKManager(option.Zkhost)
	sinkmanager.zkmanager = zkmanager

	sinkmanager.initSinkServers(option.Businesses, zkmanager)
	return sinkmanager

}

func (self *SinkManager) monitorFlume() {
	for self.isRunning {
		time.Sleep(1 * time.Second)
		monitor := "FLUME_TPS|"
		for k, v := range self.sinkServers {

			succ, fail := v.monitor()
			monitor += fmt.Sprintf("%s|%d/%d \t", k, succ, fail)
		}
		log.Println(monitor)

		mk := make([]string, 0)
		monitor = "FLUME_POOL|\n"
		for k, _ := range self.hp2flumeClientPool {
			mk = append(mk, k.Host+":"+strconv.Itoa(k.Port))
		}
		sort.Strings(mk)

		for _, hp := range mk {
			v, ok := self.hp2flumeClientPool[config.NewHostPort(hp)]
			if !ok {
				continue
			}
			active, core, max := v.FlumePool.MonitorPool()
			monitor += fmt.Sprintf("%s|%d/%d/%d\n", hp, active, core, max)
		}

		log.Println(monitor)
	}
}

func initRedisQueue(option *config.Option) map[string][]*redis.Pool {
	redisPool := make(map[string][]*redis.Pool, 0)

	//创建redis的消费连接
	for _, v := range option.QueueHostPorts {

		pool := redis.NewPool(func() (conn redis.Conn, err error) {

			conn, err = redis.DialTimeout("tcp", v.Host+":"+strconv.Itoa(v.Port),
				time.Duration(v.Timeout)*time.Second,
				time.Duration(v.Timeout)*time.Second,
				time.Duration(v.Timeout)*time.Second)

			return
		}, time.Duration(v.Timeout*2)*time.Second, v.Maxconn/2, v.Maxconn)

		pools, ok := redisPool[v.QueueName]
		if !ok {
			pools = make([]*redis.Pool, 0)
			redisPool[v.QueueName] = pools
		}
		redisPool[v.QueueName] = append(pools, pool)
	}

	return redisPool
}

func (self *SinkManager) initSinkServers(businesses []string, zkmanager *config.ZKManager) {

	for _, business := range businesses {
		nodewatcher := newFlumeWatcher(business, self)
		flumeNode := zkmanager.GetAndWatch(business, nodewatcher)
		self.watcherPool[business] = nodewatcher
		self.initSinkServer(business, flumeNode)
	}
}

func (self *SinkManager) initSinkServer(business string, flumenodes []config.HostPort) {

	//首先判断当前是否该sink支持该种business
	_, ok := self.watcherPool[business]
	if !ok {
		log.Printf("unsupport business[%s],HostPorts:[%s]\n", business, flumenodes)
		return
	}

	if len(flumenodes) == 0 {
		log.Println("no valid flume agent node for [" + business + "]")
		return
	}

	//新增的消费类型
	//使用的pool
	pools := make([]*pool.FlumePoolLink, 0)
	for _, hp := range flumenodes {
		poollink, ok := self.hp2flumeClientPool[hp]
		if !ok {
			err, tmppool := pool.NewFlumePoolLink(hp)
			if nil != err {
				log.Println("SINK_MANGER|INIT FLUMEPOOLLINE|FAIL|%s", err)
				continue
			}
			poollink = tmppool
			self.hp2flumeClientPool[hp] = poollink
		}

		defer func() {
			if nil == poollink {
				return
			}
			if err := recover(); nil != err {
				log.Fatalf("create flumeclient fail :flume:[%s]\n", hp)
				poollink = nil
			}
		}()

		if nil == poollink {
			continue
		}

		poollink.Mutex.Lock()
		poollink.BusinessLink.PushFront(business)
		pools = append(pools, poollink)
		poollink.Mutex.Unlock()
	}

	//创建一个sinkserver
	sinkserver := newSinkServer(business, self.redisPool, pools)
	sinkserver.start()
	self.sinkServers[business] = sinkserver

}

func (self *SinkManager) Start() {

	for name, v := range self.sinkServers {
		v.start()
		log.Printf("sinkserver start [%s]", name)
	}
	self.isRunning = true
	go self.monitorFlume()
}

func (self *SinkManager) Close() {
	for name, sinkserver := range self.sinkServers {
		sinkserver.stop()
		log.Printf("sinkserver stop [%s]", name)
	}

	for _, redispool := range self.redisPool {
		for _, pool := range redispool {
			pool.Close()
		}
	}

	//关闭flumepool
	for _, flumepool := range self.hp2flumeClientPool {
		flumepool.FlumePool.Destroy()
	}
	self.isRunning = false
}
