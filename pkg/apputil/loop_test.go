package apputil

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/entertainment-venue/borderland/pkg/etcdutil"
)

func Test_tickerLoop(t *testing.T) {
	var (
		wg          sync.WaitGroup
		ctx, cancel = context.WithCancel(context.Background())
	)

	wg.Add(1)
	go TickerLoop(
		ctx,
		time.Second,
		"test loop exit",
		func(ctx context.Context) error {
			fmt.Println("test fn " + time.Now().String())
			return nil
		},
	)

	go func() {
		for {
			select {
			case <-time.After(3 * time.Second):
				cancel()
			}
		}
	}()

	wg.Wait()
	fmt.Println("TestTickerLoop exit")
}

func Test_watchLoop(t *testing.T) {
	var (
		wg          sync.WaitGroup
		ctx, cancel = context.WithCancel(context.Background())
	)

	client, err := etcdutil.NewEtcdClient([]string{"127.0.0.1:2379"})
	if err != nil {
		t.Errorf("err: %v", err)
		t.SkipNow()
	}

	wg.Add(1)
	go WatchLoop(
		ctx,
		client.Client,
		"foo",
		"test loop exit",
		func(ctx context.Context, ev *clientv3.Event) error {
			fmt.Println(ev.Type)
			return nil
		},
	)

	go func() {
		for {
			select {
			case <-time.After(5 * time.Second):
				cancel()
			}
		}
	}()

	wg.Wait()
	fmt.Println("TestWatchLoop exit")
}
