package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/cloudwego/kitex/pkg/registry"
	"go.etcd.io/etcd/clientv3"
)

type etcdRegistry struct {
	etcdClient *clientv3.Client
	addrString string
	exitChan   chan struct{}
	exitOKChan chan struct{}
}

func NewEtcdRegistry(endpoints []string) (registry.Registry, error) {
	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints: endpoints,
	})
	if err != nil {
		return nil, err
	}

	s, err := getLocalIPv4Host()
	if err != nil {
		return nil, err
	}

	return &etcdRegistry{
		etcdClient: etcdClient,
		addrString: s,
		exitChan:   make(chan struct{}),
		exitOKChan: make(chan struct{}),
	}, nil
}

func NewEtcdRegistryWithAuth(endpoints []string, username, password string) (registry.Registry, error) {
	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints: endpoints,
		Username:  username,
		Password:  password,
	})
	if err != nil {
		return nil, err
	}

	s, err := getLocalIPv4Host()
	if err != nil {
		return nil, err
	}

	return &etcdRegistry{
		etcdClient: etcdClient,
		exitChan:   make(chan struct{}),
		addrString: s,
		exitOKChan: make(chan struct{}),
	}, nil
}

func (r *etcdRegistry) Register(info *registry.Info) error {
	_, p, err := net.SplitHostPort(info.Addr.String())
	if err != nil {
		panic(err)
	}
	putContent_, _ := json.Marshal(&storeInfo{
		Tags:    info.Tags,
		Network: info.Addr.Network(),
		Addr:    r.addrString + ":" + p,
		Weight:  info.Weight,
	})

	putContent := string(putContent_)

	for {
		// 租期15s
		timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()
		lease, err := r.etcdClient.Grant(timeoutCtx, 15)
		if err != nil {
			log.Printf("[kitex-registry] failed to Register(grant lease), retrying: %s\n", err)
			continue
		}

		timeoutCtx, cancel = context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()
		_, err = r.etcdClient.Put(timeoutCtx, fmt.Sprintf("kitex-registry/%s/%d",
			info.ServiceName, lease.ID), putContent, clientv3.WithLease(lease.ID))
		if err != nil {
			log.Printf("[kitex-registry] failed to Registry(put key), retrying: %s\n", err)
			continue
		}

		// 保证第一次注册成功, 将内容写入etcd, 后面起一个goroutine持续保持lease
		go func() {
			log.Println("[kitex-registry] succeed to register, now keeping lease")

			for {
				c, err := r.etcdClient.KeepAlive(context.Background(), lease.ID)
				if err != nil {
					log.Printf("[kitex-registry] failed to Registry(keepalive), retrying: %s\n", err)
					continue
				}

				time.Sleep(time.Second * 10)

				for {
					select {
					case val := <-c:
						if val == nil {
							goto retry
						}
					case <-r.exitChan:
						// 不做额外的工作, 直接退出, 让etcd自动清理
						cancel()
						r.exitOKChan <- struct{}{}
						return
					}
				}

			retry:
				//	检查退出条件
				select {
				case <-r.exitChan:
					cancel()
					r.exitChan <- struct{}{}
					return
				default:
				}

				log.Println("[kitex-registry] lease lost, retrying...")

				// 1s的冷却, 防止cpu占用骤增
				time.Sleep(time.Second)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
				defer cancel()
				lease, err := r.etcdClient.Grant(ctx, 15)
				if err != nil {
					log.Printf("[kitex-registry] failed to Register(goroutine grant lease), retrying: %s\n", err)
					goto retry
				}

				ctx, cancel = context.WithTimeout(context.Background(), time.Second*3)
				defer cancel()
				_, err = r.etcdClient.Put(ctx, fmt.Sprintf("kitex-registry/%s/%d",
					info.ServiceName, lease.ID), putContent, clientv3.WithLease(lease.ID))
				if err != nil {
					log.Printf("[kitex-registry] failed to Registry(goroutine put key), retrying: %s\n", err)
					goto retry
				}

				log.Println("[kitex-registry] succeed to register, now keeping lease")
			}
		}()

		// 保证第一次注册成功
		break
	}

	return nil

}

// 关闭相关的goroutine, 让etcd自动清理
//
//	Deregister不是重点, 不必太过关心, 只需要保证最终key被删除即可
func (r *etcdRegistry) Deregister(info *registry.Info) error {
	r.exitChan <- struct{}{}
	<-r.exitOKChan
	return nil
}

func getLocalIPv4Host() (string, error) {
	addr, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addr {
		ipNet, isIpNet := addr.(*net.IPNet)
		if isIpNet && !ipNet.IP.IsLoopback() {
			ipv4 := ipNet.IP.To4()
			if ipv4 != nil {
				return ipv4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("not found ipv4 address")
}