// Copyright 2021 The entertainment-venue Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package smserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/entertainment-venue/sm/pkg/apputil/core"
	"github.com/entertainment-venue/sm/pkg/apputil/storage"
	"github.com/entertainment-venue/sm/pkg/commonutil"
	"github.com/entertainment-venue/sm/pkg/etcdutil"
	"github.com/pkg/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

var (
	_ Shard        = new(smShard)
	_ ShardWrapper = new(smShardWrapper)
)

const (
	defaultMaxShardCount = math.MaxInt

	// defaultGuardLeaseTimeout 单位s
	// 颁发的lease要提前过期，应对clock skew，尽量保证server和client的认知一致
	defaultGuardLeaseTimeout = 15
)

// smShardWrapper 实现 ShardWrapper，4 unit test
type smShardWrapper struct{}

func (s *smShardWrapper) NewShard(c *smContainer, spec *storage.ShardSpec) (Shard, error) {
	return newSMShard(c, spec)
}

// sm的任务: 管理governedService的container和shard监控
type shardTask struct {
	GovernedService string `json:"governedService"`
}

func (t *shardTask) String() string {
	b, _ := json.Marshal(t)
	return string(b)
}

func (t *shardTask) Validate() bool {
	return t.GovernedService != ""
}

// smShard 管理某个sm app的shard
type smShard struct {
	container *smContainer
	lg        *zap.Logger
	stopper   *commonutil.GoroutineStopper

	// service 从属于leader或者sm smShard，service和container不一定一样
	service string
	// appSpec 需要通过配置影响balance算法
	appSpec *smAppSpec
	// shardSpec 分片的配置信息
	shardSpec *storage.ShardSpec

	// mpr 存储当前存活的container和shard信息，代理etcd访问
	mpr *mapper

	// operator 对接接入方，通过http请求下发shard move指令
	operator *operator

	mu sync.Mutex
	// balancing 正在rb的情况下，不能开启下一次rb
	balancing bool

	bridgeLeaseID clientv3.LeaseID
	guardLeaseID  clientv3.LeaseID

	// leaseStopper 维护guard lease的keepalive
	leaseStopper *commonutil.GoroutineStopper
	closeCh      chan struct{}
}

func newSMShard(container *smContainer, shardSpec *storage.ShardSpec) (*smShard, error) {
	ss := &smShard{
		container:    container,
		shardSpec:    shardSpec,
		stopper:      &commonutil.GoroutineStopper{},
		lg:           container.lg,
		leaseStopper: &commonutil.GoroutineStopper{},
		closeCh:      make(chan struct{}),
	}

	// 解析任务中需要负责的service
	var st shardTask
	if err := json.Unmarshal([]byte(shardSpec.Task), &st); err != nil {
		return nil, errors.Wrap(err, "")
	}
	ss.service = st.GovernedService

	// worker需要service的配置信息，作为balance的因素
	serviceSpec := container.nodeManager.ServiceSpecPath(ss.service)
	resp, err := container.Client.GetKV(context.TODO(), serviceSpec, nil)
	if err != nil {
		return nil, err
	}
	if resp.Count == 0 {
		err := errors.Errorf("service not config %s", serviceSpec)
		return nil, errors.Wrap(err, "")
	}

	appSpec := smAppSpec{}
	if err := json.Unmarshal(resp.Kvs[0].Value, &appSpec); err != nil {
		return nil, errors.Wrap(err, "")
	}
	if appSpec.MaxShardCount <= 0 {
		appSpec.MaxShardCount = defaultMaxShardCount
	}
	ss.appSpec = &appSpec

	ss.operator = newOperator(ss.lg, shardSpec.Service)
	// TODO 参数传递的有些冗余，需要重新梳理
	ss.mpr, err = newMapper(container, &appSpec, ss)
	if err != nil {
		return nil, errors.Wrap(err, "")
	}

	// 提供当前的guard lease
	leasePfx := ss.container.nodeManager.ExternalLeaseGuardPath(ss.service)
	gresp, err := ss.container.Client.Get(context.TODO(), leasePfx, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	if gresp.Count != 1 {
		err := errors.New("lease node error")
		ss.lg.Error(
			"unexpected lease count in etcd",
			zap.String("service", ss.service),
			zap.Int64("count", gresp.Count),
			zap.Error(err),
		)
		return nil, errors.Wrap(err, "")
	}
	var dv storage.Lease
	json.Unmarshal(gresp.Kvs[0].Value, &dv)
	ss.guardLeaseID = dv.ID
	ss.leaseKeepAlive(ss.guardLeaseID, defaultGuardLeaseTimeout*time.Second)

	ss.stopper.Wrap(
		func(ctx context.Context) {
			commonutil.SequenceTickerLoop(
				ctx,
				ss.lg,
				defaultLoopInterval,
				fmt.Sprintf("balanceChecker exit, service %s ", ss.service),
				func(ctx context.Context) error {
					return ss.balanceChecker(ctx)
				},
			)
		},
	)

	ss.lg.Info("smShard started", zap.String("service", ss.service))
	return ss, nil
}

func (ss *smShard) SetMaxShardCount(maxShardCount int) {
	if maxShardCount > 0 {
		ss.appSpec.MaxShardCount = maxShardCount
	}
}

func (ss *smShard) SetMaxRecoveryTime(maxRecoveryTime int) {
	if maxRecoveryTime > 0 && time.Duration(maxRecoveryTime)*time.Second <= maxRecoveryWaitTime {
		ss.mpr.maxRecoveryTime = time.Duration(maxRecoveryTime) * time.Second
	}
}

func (ss *smShard) Spec() *storage.ShardSpec {
	return ss.shardSpec
}

func (ss *smShard) Close() error {
	close(ss.closeCh)
	ss.mpr.Close()

	ss.stopper.Close()
	ss.leaseStopper.Close()

	ss.lg.Info(
		"smShard closed",
		zap.String("service", ss.service),
	)
	return nil
}

// 1 smContainer 的增加/减少是优先级最高，目前可能涉及大量shard move
// 2 smShard 被漏掉作为container检测的补充，最后校验，这种情况只涉及到漏掉的shard任务下发下去
func (ss *smShard) balanceChecker(ctx context.Context) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	// rb中，没必要再次rb
	// 当前进行的rb要根据container存活现状（方法调用时刻的一个切面）计算策略
	if ss.balancing {
		ss.lg.Info("balancing", zap.String("service", ss.service))
		return nil
	}
	ss.balancing = true
	defer func() {
		ss.balancing = false
	}()

	// 判断lease是否过期,如果lease过期,需要触发一次rb来更新lease
	lease := clientv3.NewLease(ss.container.Client.GetClient().Client)
	liveResp, erre := lease.TimeToLive(context.TODO(), ss.guardLeaseID)
	if erre != nil {
		return errors.Wrap(erre, "")
	}
	if liveResp.TTL == -1 {
		ss.lg.Info(
			"current guard lease expired, will rb to renew lease",
			zap.String("service", ss.service),
			zap.Int64("guardLease", int64(ss.guardLeaseID)),
		)
		if err := ss.rb(moveActionList{}); err != nil {
			ss.lg.Error(
				"rb to renew lease failed",
				zap.String("service", ss.service),
				zap.Int64("guardLease", int64(ss.guardLeaseID)),
				zap.Error(err),
			)
			return errors.Wrap(err, "")
		}
	}

	// 获取guard lease
	if err := ss.validateGuardLease(); err != nil {
		ss.lg.Error(
			"validateGuardLease error",
			zap.String("service", ss.service),
			zap.Error(err),
		)
		return err
	}

	// 现有存活containers
	etcdHbContainerIdAndAny := ss.mpr.AliveContainers()
	// 没有存活的container，不需要做shard移动
	if len(etcdHbContainerIdAndAny) == 0 {
		ss.lg.Info(
			"no alive container",
			zap.String("service", ss.service),
		)
		return nil
	}

	// 获取当前所有shard配置
	var (
		etcdShardIdAndAny ArmorMap
		err               error

		// 针对特定service，区分group做rb，允许同一service可以划分多个小的业务场景
		groups = newBalanceWorkerGroupManager()
	)
	// shard可以指定分配到某一个workerGroup,一个workerGroup可以包含多个container
	workerGroupAndContainers, err := ss.getHbWorkerGroupAndContainers(etcdHbContainerIdAndAny)
	if err != nil {
		return err
	}
	shardKey := ss.container.nodeManager.ShardDir(ss.service)
	etcdShardIdAndAny, err = ss.container.Client.GetKVs(ctx, shardKey)
	if err != nil {
		return err
	}
	// 提供给 moveAction，做内容下发，防止sdk再次获取，sdk不会有sm空间的访问权限
	shardIdAndShardSpec := make(map[string]*storage.ShardSpec)
	for id, value := range etcdShardIdAndAny {
		var ssc storage.ShardSpec
		if err := json.Unmarshal([]byte(value), &ssc); err != nil {
			return errors.Wrap(err, "")
		}

		// shard手动指定的container不存活则不参与rb
		if ssc.ManualContainerId != "" {
			if _, ok := etcdHbContainerIdAndAny[ssc.ManualContainerId]; !ok {
				ss.lg.Error(
					"shard manualContainerId not alive",
					zap.String("service", ss.service),
					zap.String("shardId", id),
					zap.String("manualContainerId", ssc.ManualContainerId),
				)
				delete(etcdShardIdAndAny, id)
				continue
			}
			// 将shard的workerGroup置为空，防止出现不存在的workerGroup影响下面的逻辑
			ssc.WorkerGroup = ""
		} else {
			// shard手动指定的优先级高于workerGroup
			// shard的workerGroup不存在则不参与rb
			if _, ok := workerGroupAndContainers[ssc.WorkerGroup]; !ok {
				ss.lg.Error(
					"shard worker group no alive containers",
					zap.String("service", ss.service),
					zap.String("shardId", id),
					zap.String("workerGroup", ssc.WorkerGroup),
					zap.Reflect("alive workerGroup containers", workerGroupAndContainers),
				)
				delete(etcdShardIdAndAny, id)
				continue
			}
		}
		shardIdAndShardSpec[id] = &ssc
		// 按照group聚合
		groups.addShard(id, ssc.ManualContainerId, ssc.Group, ssc.WorkerGroup)
	}

	// 获取当前存活shard，存活shard的container分配关系如果命中可以不生产moveAction
	etcdHbShardIdAndValue := ss.mpr.AliveShards()

	// allShardMoves 收集所有的moveAction，用于在checker最后做guard lease的机制
	var allShardMoves moveActionList

	// shard被清除的场景，从rebalance方法中提前到这里，应对完全不配置shard，且sdk本地存活的场景
	// 提取需要被移除的shard
	var deleting moveActionList
	for hbShardId, value := range etcdHbShardIdAndValue {
		// shard配置不存在，需要删除
		spec, ok := shardIdAndShardSpec[hbShardId]
		if !ok {
			deleting = append(
				deleting,
				&moveAction{
					Service:      ss.service,
					ShardId:      hbShardId,
					DropEndpoint: value.curContainerId,
				},
			)
			delete(etcdHbShardIdAndValue, hbShardId)
			continue
		}

		// shard当前的container和指定的container不一致，需要删除
		mci := shardIdAndShardSpec[hbShardId].ManualContainerId
		if mci != "" && value.curContainerId != spec.ManualContainerId {
			deleting = append(
				deleting,
				&moveAction{
					Service:      ss.service,
					ShardId:      hbShardId,
					DropEndpoint: value.curContainerId,
				},
			)
			delete(etcdHbShardIdAndValue, hbShardId)
			continue
		}

		// shard所属的workerGroup的containers不包含shard所在的container，需要删除
		if cs, ok := workerGroupAndContainers[spec.WorkerGroup]; ok {
			if _, ok := cs[value.curContainerId]; !ok {
				deleting = append(
					deleting,
					&moveAction{
						Service:      ss.service,
						ShardId:      hbShardId,
						DropEndpoint: value.curContainerId,
					},
				)
				delete(etcdHbShardIdAndValue, hbShardId)
				continue
			}
		}
		groups.addHbShard(hbShardId, value.curContainerId, spec.Group, spec.WorkerGroup)
	}

	if len(deleting) > 0 {
		allShardMoves = append(allShardMoves, deleting...)
		ss.lg.Info(
			"shard deleting",
			zap.String("service", ss.service),
			zap.Reflect("deleting", deleting),
		)
	} else {
		// shard被清理，但是发现心跳带上来的还有未清理掉的shard，通过rb drop掉
		if len(etcdShardIdAndAny) == 0 {
			ss.lg.Info(
				"no shard configured",
				zap.String("service", ss.service),
			)
			return nil
		}
	}

	// 增加workerGroup粒度阈值限制，防止单进程过载导致雪崩
	//workerGroupShards := make(map[string]int)
	//for shardId := range etcdShardIdAndAny {
	//	workerGroupShards[shardIdAndShardSpec[shardId].WorkerGroup] = workerGroupShards[shardIdAndShardSpec[shardId].WorkerGroup] + 1
	//}
	//for wg, shardCnt := range workerGroupShards {
	//	maxHold := ss.maxHold(len(workerGroupAndContainers[wg]), shardCnt)
	//	if maxHold > ss.appSpec.MaxShardCount {
	//		err := errors.New("MaxShardCount exceeded")
	//		ss.lg.Error(
	//			err.Error(),
	//			zap.String("service", ss.service),
	//			zap.String("workerGroup", wg),
	//			zap.Int("maxHold", maxHold),
	//			zap.Int("containerCnt", len(workerGroupAndContainers[wg])),
	//			zap.Int("shardCnt", shardCnt),
	//			zap.Error(err),
	//		)
	//		// 一个资源组的分配超过最大限制，不影响其他资源组的分配
	//		delete(workerGroupShards, wg)
	//	}
	//}

	// 现存shard的分配
	for groupkey, bg := range groups.balancerGroup {
		group, wGroup := getGroupAndWorkerGroupByKey(groupkey)
		hbContainerIds := workerGroupAndContainers[wGroup].KeyList()
		fixShardIds := bg.fixShardIdAndManualContainerId.KeyList()
		hbShardIds := bg.hbShardIdAndContainerId.KeyList()
		// 没有存活分片，且没有分片待分配
		if len(fixShardIds) == 0 && len(hbShardIds) == 0 {
			ss.lg.Warn(
				"no survive shard",
				zap.String("group", group),
				zap.String("service", ss.service),
			)
			continue
		}

		containerChanged := ss.changed(hbContainerIds, bg.hbShardIdAndContainerId.ValueList())
		shardChanged := ss.changed(fixShardIds, hbShardIds)
		if !containerChanged && !shardChanged {
			continue
		}

		ss.lg.Info(
			"changed",
			zap.String("group", group),
			zap.String("workerGroup", wGroup),
			zap.String("service", ss.service),
			zap.Bool("containerChanged", containerChanged),
			zap.Bool("shardChanged", shardChanged),
			zap.Reflect("hbContainerIds", hbContainerIds),
			zap.Reflect("bg.hbShardIdAndContainerId.ValueList())", bg.hbShardIdAndContainerId.ValueList()),
			zap.Reflect("fixShardIds", fixShardIds),
			zap.Reflect("hbShardIds", hbShardIds),
		)

		shardMoves := ss.extractShardMoves(bg.fixShardIdAndManualContainerId, workerGroupAndContainers[wGroup], bg.hbShardIdAndContainerId, shardIdAndShardSpec)
		if len(shardMoves) > 0 {
			allShardMoves = append(allShardMoves, shardMoves...)
			continue
		}
		// 当survive的container为nil的时候，不能形成有效的分配，直接返回即可
		ss.lg.Warn(
			"can not extractShardMoves",
			zap.String("service", ss.service),
			zap.Bool("container-changed", containerChanged),
			zap.Bool("shard-changed", shardChanged),
			zap.String("group", group),
			zap.Reflect("shardIdAndManualContainerId", bg.fixShardIdAndManualContainerId),
			zap.Strings("etcdHbContainerIds", etcdHbContainerIdAndAny.KeyList()),
			zap.Reflect("hbShardIdAndContainerId", bg.hbShardIdAndContainerId),
		)

	}

	// guard lease 实现
	if len(allShardMoves) > 0 {
		ss.lg.Info(
			"service start rb",
			zap.String("service", ss.service),
		)
		return ss.rb(allShardMoves)
	}
	return nil
}

func (ss *smShard) rb(shardMoves moveActionList) error {
	if _, err := ss.container.Client.Delete(context.TODO(), ss.container.nodeManager.ExternalLeaseBridgePath(ss.service)); err != nil {
		return err
	}

	// 1 先梳理出来drop的shard
	assignment := core.Assignment{}
	for _, action := range shardMoves {
		// 涉及到移动的分片，都需要公布出来，防止以下情况，
		assignment.Drops = append(assignment.Drops, action.ShardId)
	}
	// 2 获取新的bridge lease，bridge lease的过期不应该和rb绑定，可以通过时间触发分离出去，让程序机制更合理
	// 这里是defaultGuardLeaseTimeout的2倍，原因：
	// a bridge颁发，需要等待一个10s，old guard -> bridge & old guard expired
	// b new guard颁发，再等待10s，bridge -> new guard & bridge expired
	// 上面的expired是保证一致性的关键，让server端能够确认client的行为，利用time clock得到一个基本正确的结论，除非clock skew过大
	blease := clientv3.NewLease(ss.container.Client.GetClient().Client)
	bridgeGrantLeaseResp, err := blease.Grant(context.TODO(), 2*defaultGuardLeaseTimeout)
	if err != nil {
		return errors.Wrap(err, "Grant error")
	}
	ss.bridgeLeaseID = bridgeGrantLeaseResp.ID
	defer func() {
		ss.bridgeLeaseID = clientv3.NoLease
	}()
	// 3 写入bridge lease节点
	bridgeLease := core.ShardLease{
		GuardLeaseID: ss.guardLeaseID,
		Assignment:   &assignment,
	}
	bridgeLease.ID = bridgeGrantLeaseResp.ID
	// 注意这个 Expire 不需要特别精确，保证server的时间戳加上一个时间段，大部分client都会快速切换到bridge上，少部分慢的，也通过下面的sleep保证过期掉
	bridgeLease.Expire = time.Now().Unix() + bridgeGrantLeaseResp.TTL
	bridgePfx := ss.container.nodeManager.ExternalLeaseBridgePath(ss.service)
	if err := ss.container.Client.CreateAndGet(context.TODO(), []string{bridgePfx}, []string{bridgeLease.String()}, bridgeGrantLeaseResp.ID); err != nil {
		return err
	}
	ss.lg.Info(
		"bridge created",
		zap.String("service", ss.service),
		zap.Int64("new-bridge-lease", int64(bridgeLease.ID)),
	)

	// 4 等待客户端lease确定超时，客户端将old guard lease的shard都停止工作，最长停止10s，也就是在bridge lease下发之后立即

	// old guard lease不继续续约，etcd中的数据不在刷新，会导致Expire停止更新，下面的等待就能保证切换到bridge有延迟的shard被drop掉
	ss.leaseStopper.Close()

	ss.lg.Info(
		"waiting old guard expired",
		zap.Int64("oldGuardLease", int64(ss.guardLeaseID)),
		zap.String("currentBridgePfx", bridgePfx),
		zap.Reflect("currentBridgeLease", bridgeLease),
	)
	// 颁发bridge的前提下，等待旧的guard lease过期掉，client会提前2s过期，按照目前的设定，8s过期，这块其实就是lease的核心保证，也是依赖
	// 服务器时钟同步的点。
	// 这个就是slicer论文4.5中提到的delay：
	// The Assigner writes and distributes assignment
	// A2, creates the bridge lease, delays for Slicelets to acquire the bridge lease for reading, and only then does it
	// recall and rewrite the guard lease.
	commonutil.SleepCanClose(defaultGuardLeaseTimeout*time.Second, ss.closeCh)
	ss.lg.Info(
		"old guard expired",
		zap.Int64("oldGuardLease", int64(ss.guardLeaseID)),
		zap.String("currentBridgePfx", bridgePfx),
		zap.Reflect("currentBridgeLease", bridgeLease),
	)

	// 5 grant guard lease，这里的lease不和guard节点存活挂钩，只用到leaseID，所以timeout是多少暂时无所谓，如果关联guard lease，涉及到改动太大包括shardKeeper
	glease := clientv3.NewLease(ss.container.Client.GetClient().Client)
	guardLeaseResp, err := glease.Grant(context.TODO(), defaultGuardLeaseTimeout)
	if err != nil {
		return errors.Wrap(err, "Grant error")
	}
	// 刷新内存的guard lease
	ss.guardLeaseID = guardLeaseResp.ID
	// 6 设置guard lease，lease的过期不会影响到guard lease节点，所以Expire设置的有误差
	guardPfx := ss.container.nodeManager.ExternalLeaseGuardPath(ss.service)
	guardLease := storage.Lease{
		ID: guardLeaseResp.ID,

		// guard lease改为使用clientv3内部的KeepAlive，这块改为上限，让旧的客户端lease功能失效，依赖session的close作为shard drop的依据。
		// -30的原因是，shardkeeper有2s的buf提前过期本地的shard，直接使用MaxInt64会导致溢出
		Expire: math.MaxInt64 - 30,
	}
	if _, err := ss.container.Client.Put(context.TODO(), guardPfx, guardLease.String()); err != nil {
		return err
	}

	// guard lease需要定时续约，把Expire延长，防止app清除掉shard
	ss.leaseKeepAlive(ss.guardLeaseID, etcdutil.DefaultRequestTimeout)

	// 7 等待bridge到guard迁移，以及bridge expired，足够网络健康状态下的所有节点的shard迁移
	ss.lg.Info(
		"waiting bridge expired",
		zap.String("guardPfx", guardPfx),
		zap.Reflect("currentBridgeLease", bridgeLease),
		zap.Reflect("newGuardLease", guardLease),
	)
	commonutil.SleepCanClose(defaultGuardLeaseTimeout*time.Second, ss.closeCh)
	ss.lg.Info(
		"bridge expired",
		zap.String("guardPfx", guardPfx),
		zap.Reflect("currentBridgeLease", bridgeLease),
		zap.Reflect("newGuardLease", guardLease),
	)

	// 8 下发add请求
	var addMALs moveActionList
	shards := ss.mpr.AliveShards()
	for _, action := range shardMoves {
		if action.AddEndpoint == "" {
			continue
		}

		t, ok := shards[action.ShardId]
		if !ok {
			// 不是存活shard，可以移动
			action.DropEndpoint = ""
			action.Spec.Lease = &storage.Lease{
				ID: guardLease.ID,

				// 旧shardkeeper，在bridge过期的之后，会干掉lease比bridge还早的shard，这些shard灭有切换到new guard上。
				// 给1的原因是，兼容旧的根据bridge进行drop的逻辑，防止直接走到ID大小的判断上来，误drop掉shard，具体可以参考shardkeeper.go
				// TODO 升级完sdk之后，去掉这块逻辑
				Expire: bridgeLease.Expire + 1,
			}
			addMALs = append(addMALs, action)
			continue
		}

		// shard存活，又需要移动，不太可能
		ss.lg.Warn(
			"alive shard valid lease, but rb include this shard's add action",
			zap.String("shardId", action.ShardId),
			zap.Int64("curLeaseID", int64(t.leaseID)),
			zap.Int64("guardLease", int64(guardLease.ID)),
			zap.Reflect("t", t),
		)
	}
	// http请求成功，boltdb中就会记录新的shard，这个shard会随着下一个heartbeat上报上来，这块http异步走，可能和下次rb冲突，case变得复杂。
	// 并发或者同步会把延迟算到guard lease的下次续约延时中，也会影响系统稳定性。所以这块需要做lease keepalive。
	if err := ss.dispatchMALs(addMALs); err != nil {
		ss.lg.Error(
			"dispatchMALs error",
			zap.String("service", ss.service),
			zap.Reflect("mal", addMALs),
			zap.Error(err),
		)
		return err
	}

	// 4 debug，日志排查困难
	commonutil.SleepCanClose(3*time.Second, ss.closeCh)

	return nil
}

// leaseKeepAlive
// proposal https://github.com/entertainment-venue/sm/wiki/%5Bproposal%5D%E4%BD%BF%E7%94%A8clientv3%E4%B8%AD%E7%9A%84lease%E7%9A%84keepalive%E6%9B%BF%E4%BB%A3guardKeepaliver%E6%9C%BA%E5%88%B6
func (ss *smShard) leaseKeepAlive(leaseID clientv3.LeaseID, leaseTimeout time.Duration) {
	if leaseID == clientv3.NoLease {
		ss.lg.Info(
			"leaseKeepAlive can not started, leaseID is 0",
			zap.String("service", ss.service),
			zap.Int64("lease-id", int64(leaseID)),
		)
		return
	}

	lease := clientv3.NewLease(ss.container.Client.GetClient().Client)
	ss.leaseStopper.Wrap(
		func(ctx context.Context) {
			commonutil.TickerLoop(
				ctx,
				ss.lg,
				3*time.Second,
				fmt.Sprintf("leaseStopper exit %s", ss.service),
				func(ctx context.Context) error {
					ctx1, cancel := context.WithTimeout(ctx, leaseTimeout)
					defer cancel()
					res, err := lease.KeepAliveOnce(ctx1, leaseID)
					if err != nil {
						// 如果client不开启dropExpiredShard，这个报错不会造成影响，如果开启，那么client会先drop掉shard，触发rb，然后server端
						// 重启leaseKeepAlive
						ss.lg.Warn(
							"failed to KeepAliveOnce",
							zap.String("service", ss.service),
							zap.Int64("lease-id", int64(leaseID)),
							zap.Error(err),
						)
						return nil
					}

					ss.lg.Info(
						"leaseKeepAlive looping",
						zap.String("service", ss.service),
						zap.Int64("lease-id", int64(leaseID)),
						zap.Int64("TTL", res.TTL),
					)

					return nil
				},
			)
		},
	)
	ss.lg.Info(
		"leaseKeepAlive started",
		zap.String("service", ss.service),
	)
}

func (ss *smShard) validateGuardLease() error {
	// 获取guard lease
	guardPfx := ss.container.nodeManager.ExternalLeaseGuardPath(ss.service)
	resp, err := ss.container.Client.GetKV(context.TODO(), guardPfx, []clientv3.OpOption{clientv3.WithPrefix()})
	if err != nil {
		return err
	}
	if resp.Count == 0 {
		err := errors.Errorf("guard not found %s", guardPfx)
		return errors.Wrap(err, "")
	}
	var lease storage.Lease
	if err := json.Unmarshal(resp.Kvs[0].Value, &lease); err != nil {
		return errors.Wrap(err, "")
	}
	// if lease.ID == clientv3.NoLease {
	// 	ss.lg.Info(
	// 		"zero guard lease",
	// 		zap.String("guardPfx", guardPfx),
	// 	)
	// } else {
	// 	ss.lg.Info(
	// 		"current guard lease",
	// 		zap.String("guardPfx", guardPfx),
	// 		zap.Int64("leaseID", int64(lease.ID)),
	// 	)
	// }
	return nil
}

func (ss *smShard) changed(a []string, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	return !reflect.DeepEqual(a, b)
}

// 只负责shard移动的场景，删除在balanceChecker中处理
func (ss *smShard) extractShardMoves(
	fixShardIdAndManualContainerId ArmorMap,
	hbContainerIdAndAny ArmorMap,
	hbShardIdAndContainerId ArmorMap,
	shardIdAndShardSpec map[string]*storage.ShardSpec) moveActionList {
	// 保证shard在hb中上报的container和存活container一致
	containerIdAndHbShardIds := hbShardIdAndContainerId.SwapKV()
	for containerId := range containerIdAndHbShardIds {
		_, ok := hbContainerIdAndAny[containerId]
		if !ok {
			ss.lg.Error(
				"container in shard heartbeat do not exist in container heartbeat",
				zap.String("containerId", containerId),
			)
			return nil
		}
	}

	var (
		mals moveActionList

		// 在最后做shard分配的时候合并到大集合中
		adding []string

		br = &balancer{
			bcs: make(map[string]*balancerContainer),
		}
	)

	// 构建container和shard的关系
	for fixShardId, manualContainerId := range fixShardIdAndManualContainerId {
		// 不在container上，可能是新增，确定需要被分配
		currentContainerId, ok := hbShardIdAndContainerId[fixShardId]
		if !ok {
			if manualContainerId != "" {
				spec := shardIdAndShardSpec[fixShardId]
				mals = append(
					mals,
					&moveAction{
						Service:     ss.service,
						ShardId:     fixShardId,
						AddEndpoint: manualContainerId,
						Spec:        spec,
					},
				)

				// 确定的指令，要对当前的csm有影响
				br.put(manualContainerId, fixShardId, true)
			} else {
				adding = append(adding, fixShardId)
			}
			continue
		}

		// 不在要求的container上
		if manualContainerId != "" {
			if currentContainerId != manualContainerId {
				spec := shardIdAndShardSpec[fixShardId]
				mals = append(
					mals,
					&moveAction{
						Service:      ss.service,
						ShardId:      fixShardId,
						DropEndpoint: currentContainerId,
						AddEndpoint:  manualContainerId,
						Spec:         spec,
					},
				)

				// 确定的指令，要对当前的csm有影响
				br.put(manualContainerId, fixShardId, true)
			} else {
				// 命中manual是不能被移动的
				br.put(currentContainerId, fixShardId, true)
			}
			continue
		}

		br.put(currentContainerId, fixShardId, false)
	}

	// 处理新增container
	for hbContainerId := range hbContainerIdAndAny {
		_, ok := containerIdAndHbShardIds[hbContainerId]
		if !ok {
			br.addContainer(hbContainerId)
		}
	}

	shardLen := len(fixShardIdAndManualContainerId)
	containerLen := len(hbContainerIdAndAny)

	// 每个container最少包含多少shard
	maxHold := ss.maxHold(containerLen, shardLen)

	dropFroms := make(map[string]string)
	getDrops := func(bc *balancerContainer) {
		dropCnt := len(bc.shards) - maxHold
		if dropCnt <= 0 {
			return
		}

		for _, bs := range bc.shards {
			// 不能变动的shard
			if bs.isManual {
				continue
			}
			dropFroms[bs.id] = bc.id
			delete(bc.shards, bs.id)
			dropCnt--
			if dropCnt == 0 {
				break
			}
		}
	}
	br.forEach(getDrops)

	// 可以移动的shard，补充到待分配中
	for drop := range dropFroms {
		adding = append(adding, drop)
	}
	if len(adding) > 0 {
		add := func(bc *balancerContainer) {
			addCnt := maxHold - len(bc.shards)
			if addCnt <= 0 {
				return
			}

			idx := 0
			for {
				if idx == addCnt || idx == len(adding) {
					break
				}

				shardId := adding[idx]
				spec := shardIdAndShardSpec[shardId]
				from, ok := dropFroms[shardId]
				if ok {
					mals = append(
						mals,
						&moveAction{
							Service:      ss.service,
							ShardId:      adding[idx],
							DropEndpoint: from,
							AddEndpoint:  bc.id,
							Spec:         spec,
						},
					)
				} else {
					mals = append(
						mals,
						&moveAction{
							Service:     ss.service,
							ShardId:     adding[idx],
							AddEndpoint: bc.id,
							Spec:        spec,
						},
					)
				}
				idx++
			}
			adding = adding[idx:]
		}
		br.forEach(add)
	}

	ss.lg.Info(
		"rb result",
		zap.String("service", ss.service),
		zap.Reflect("resultMAL", mals),
		zap.Any("fixShardIdAndManualContainerId", fixShardIdAndManualContainerId),
		zap.Strings("hbContainerIdAndAny", hbContainerIdAndAny.KeyList()),
		zap.Any("hbShardIdAndContainerId", hbShardIdAndContainerId),
	)
	return mals
}

func (ss *smShard) maxHold(containerCnt, shardCnt int) int {
	if containerCnt == 0 {
		// 不做过滤
		return 0
	}
	base := shardCnt / containerCnt
	delta := shardCnt % containerCnt
	var r int
	if delta > 0 {
		r = base + 1
	} else {
		r = base
	}
	return r
}

func (ss *smShard) dispatchMALs(mal moveActionList) error {
	if len(mal) == 0 {
		ss.lg.Warn(
			"empty mal",
			zap.String("service", ss.service),
		)
		return nil
	}
	return ss.operator.move(mal)
}

// 获取存在心跳的WorkerGroup的Containers
func (ss *smShard) getHbWorkerGroupAndContainers(hbContainers ArmorMap) (map[string]ArmorMap, error) {
	wgc := make(map[string]ArmorMap)
	// workerGroup为空的时候，所有的container都符合
	wgc[""] = hbContainers
	pfx := ss.container.nodeManager.WorkerGroupPath(ss.service)
	resp, err := ss.container.Client.Get(context.TODO(), pfx, clientv3.WithPrefix())
	if err != nil {
		ss.lg.Error(
			"GetKVs error",
			zap.String("service", ss.service),
			zap.String("pfx", pfx),
			zap.Error(err),
		)
		return nil, errors.Wrap(err, "")
	}
	for _, kv := range resp.Kvs {
		// /sm/app/foo.bar/service/foo.bar/workerpool/g1/127.0.0.1:8801
		wGroup, container := ss.container.nodeManager.parseWorkerGroupAndContainer(string(kv.Key))

		// container存活
		if _, ok := hbContainers[container]; ok {
			cs := wgc[wGroup]
			if cs == nil {
				wgc[wGroup] = make(ArmorMap)
			}
			wgc[wGroup][container] = ""
		}
	}
	return wgc, nil
}
