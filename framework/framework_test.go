package framework

import (
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/coreos/go-etcd/etcd"
	"github.com/go-distributed/meritop"
	"github.com/go-distributed/meritop/controller"
	"github.com/go-distributed/meritop/example"
	"github.com/go-distributed/meritop/framework/frameworkhttp"
	"github.com/go-distributed/meritop/pkg/etcdutil"
)

// TestRequestDataEpochMismatch creates a scenario where data request happened
// with two different epochs. In this case, the server should back pressure and
// request client should get notified and return error.
func TestRequestDataEpochMismatch(t *testing.T) {
	job := "TestRequestDataEpochMismatch"
	m := etcdutil.StartNewEtcdServer(t, job)
	defer m.Terminate(t)
	etcdURLs := []string{m.URL()}
	controller := controller.New(job, etcd.NewClient(etcdURLs), 1)
	controller.Start()
	defer controller.Stop()

	fw := &framework{
		name:     job,
		etcdURLs: etcdURLs,
		ln:       createListener(t),
	}
	var wg sync.WaitGroup
	fw.SetTaskBuilder(&testableTaskBuilder{
		setupLatch: &wg,
	})
	fw.SetTopology(example.NewTreeTopology(1, 1))
	wg.Add(1)
	go fw.Start()
	defer fw.ShutdownJob()
	wg.Wait()

	addr, err := etcdutil.GetAddress(fw.etcdClient, job, fw.GetTaskID())
	if err != nil {
		t.Fatalf("GetAddress failed: %v", err)
	}
	_, err = frameworkhttp.RequestData(addr, "req", 0, fw.GetTaskID(), 10, fw.GetLogger())
	// if err.Error() != "epoch mismatch" {
	if err != frameworkhttp.ErrReqEpochMismatch {
		t.Fatalf("error want = (epoch mismatch), but get = (%s)", err.Error())
	}
}

// TestFrameworkFlagMetaReady and TestFrameworkDataRequest test basic workflows of
// framework impl. It uses a scenario with two nodes: 0 as parent, 1 as child.
// The basic idea is that when parent tries to talk to child and vice versa,
// there will be some data transferring and captured by application task.
// Here we have implemented a helper user task to capture those data, test if
// it's passed from framework correctly and unmodified.
func TestFrameworkFlagMetaReady(t *testing.T) {
	appName := "framework_test_flagmetaready"
	// launch testing etcd server
	m := etcdutil.MustNewMember(t, appName)
	m.Launch()
	defer m.Terminate(t)
	url := fmt.Sprintf("http://%s", m.ClientListeners[0].Addr().String())

	// launch controller to setup etcd layout
	ctl := controller.New(appName, etcd.NewClient([]string{url}), 2)
	if err := ctl.InitEtcdLayout(); err != nil {
		t.Fatalf("initEtcdLayout failed: %v", err)
	}
	defer ctl.DestroyEtcdLayout()

	pDataChan := make(chan *tDataBundle, 1)
	cDataChan := make(chan *tDataBundle, 1)

	// simulate two tasks on two nodes -- 0 and 1
	// 0 is parent, 1 is child
	f0 := &framework{
		name:     appName,
		etcdURLs: []string{url},
		ln:       createListener(t),
	}
	f1 := &framework{
		name:     appName,
		etcdURLs: []string{url},
		ln:       createListener(t),
	}

	var wg sync.WaitGroup
	taskBuilder := &testableTaskBuilder{
		dataMap:    nil,
		cDataChan:  cDataChan,
		pDataChan:  pDataChan,
		setupLatch: &wg,
	}
	f0.SetTaskBuilder(taskBuilder)
	f0.SetTopology(example.NewTreeTopology(2, 2))
	f1.SetTaskBuilder(taskBuilder)
	f1.SetTopology(example.NewTreeTopology(2, 2))

	taskBuilder.setupLatch.Add(2)
	go f0.Start()
	go f1.Start()
	taskBuilder.setupLatch.Wait()
	if f0.GetTaskID() != 0 {
		f0, f1 = f1, f0
	}

	defer f0.ShutdownJob()

	tests := []struct {
		cMeta string
		pMeta string
	}{
		{"parent", "child"},
		{"ParamReady", "GradientReady"},
	}

	for i, tt := range tests {
		// 0: F#FlagChildMetaReady -> 1: T#ParentMetaReady
		f0.FlagMetaToChild(tt.cMeta)
		// from child(1)'s view
		data := <-pDataChan
		expected := &tDataBundle{0, tt.cMeta, "", nil}
		if !reflect.DeepEqual(data, expected) {
			t.Errorf("#%d: data bundle want = %v, get = %v", i, expected, data)
		}

		// 1: F#FlagParentMetaReady -> 0: T#ChildMetaReady
		f1.FlagMetaToParent(tt.pMeta)
		// from parent(0)'s view
		data = <-cDataChan
		expected = &tDataBundle{1, tt.pMeta, "", nil}
		if !reflect.DeepEqual(data, expected) {
			t.Errorf("#%d: data bundle want = %v, get = %v", i, expected, data)
		}
	}
}

func TestFrameworkDataRequest(t *testing.T) {
	appName := "framework_test_flagmetaready"
	// launch testing etcd server
	m := etcdutil.MustNewMember(t, appName)
	m.Launch()
	defer m.Terminate(t)
	url := fmt.Sprintf("http://%s", m.ClientListeners[0].Addr().String())

	// launch controller to setup etcd layout
	ctl := controller.New(appName, etcd.NewClient([]string{url}), 2)
	if err := ctl.InitEtcdLayout(); err != nil {
		t.Fatalf("initEtcdLayout failed: %v", err)
	}
	defer ctl.DestroyEtcdLayout()

	tests := []struct {
		req  string
		resp []byte
	}{
		{"request", []byte("response")},
		{"parameters", []byte{1, 2, 3}},
		{"gradient", []byte{4, 5, 6}},
	}

	dataMap := make(map[string][]byte)
	for _, tt := range tests {
		dataMap[tt.req] = tt.resp
	}

	pDataChan := make(chan *tDataBundle, 1)
	cDataChan := make(chan *tDataBundle, 1)
	// simulate two tasks on two nodes -- 0 and 1
	// 0 is parent, 1 is child
	f0 := &framework{
		name:     appName,
		etcdURLs: []string{url},
		ln:       createListener(t),
	}
	f1 := &framework{
		name:     appName,
		etcdURLs: []string{url},
		ln:       createListener(t),
	}

	var wg sync.WaitGroup
	taskBuilder := &testableTaskBuilder{
		dataMap:    dataMap,
		cDataChan:  cDataChan,
		pDataChan:  pDataChan,
		setupLatch: &wg,
	}
	f0.SetTaskBuilder(taskBuilder)
	f0.SetTopology(example.NewTreeTopology(2, 2))
	f1.SetTaskBuilder(taskBuilder)
	f1.SetTopology(example.NewTreeTopology(2, 2))

	taskBuilder.setupLatch.Add(2)
	go f0.Start()
	go f1.Start()
	taskBuilder.setupLatch.Wait()
	if f0.GetTaskID() != 0 {
		f0, f1 = f1, f0
	}

	defer f0.ShutdownJob()

	for i, tt := range tests {
		// 0: F#DataRequest -> 1: T#ServeAsChild -> 0: T#ChildDataReady
		f0.DataRequest(1, tt.req)
		// from child(1)'s view at 1: T#ServeAsChild
		data := <-pDataChan
		expected := &tDataBundle{0, "", data.req, nil}
		if !reflect.DeepEqual(data, expected) {
			t.Errorf("#%d: data bundle want = %v, get = %v", i, expected, data)
		}
		// from parent(0)'s view at 0: T#ChildDataReady
		data = <-cDataChan
		expected = &tDataBundle{1, "", data.req, data.resp}
		if !reflect.DeepEqual(data, expected) {
			t.Errorf("#%d: data bundle want = %v, get = %v", i, expected, data)
		}

		// 1: F#DataRequest -> 0: T#ServeAsParent -> 1: T#ParentDataReady
		f1.DataRequest(0, tt.req)
		// from parent(0)'s view at 0: T#ServeAsParent
		data = <-cDataChan
		expected = &tDataBundle{1, "", data.req, nil}
		if !reflect.DeepEqual(data, expected) {
			t.Errorf("#%d: data bundle want = %v, get = %v", i, expected, data)
		}
		// from child(1)'s view at 1: T#ParentDataReady
		data = <-pDataChan
		expected = &tDataBundle{0, "", data.req, data.resp}
		if !reflect.DeepEqual(data, expected) {
			t.Errorf("#%d: data bundle want = %v, get = %v", i, expected, data)
		}
	}
}

type tDataBundle struct {
	id   uint64
	meta string
	req  string
	resp []byte
}

type testableTaskBuilder struct {
	dataMap    map[string][]byte
	cDataChan  chan *tDataBundle
	pDataChan  chan *tDataBundle
	setupLatch *sync.WaitGroup
}

func (b *testableTaskBuilder) GetTask(taskID uint64) meritop.Task {
	switch taskID {
	case 0:
		return &testableTask{dataMap: b.dataMap, dataChan: b.cDataChan,
			setupLatch: b.setupLatch}
	case 1:
		return &testableTask{dataMap: b.dataMap, dataChan: b.pDataChan,
			setupLatch: b.setupLatch}
	default:
		panic("unimplemented")
	}
}

type testableTask struct {
	id         uint64
	framework  meritop.Framework
	setupLatch *sync.WaitGroup
	// dataMap will be used to serve data according to request
	dataMap map[string][]byte

	// This channel is used to convey data passed from framework back to the main
	// thread, for checking. Thus it's initialized and passed in from outside.
	//
	// The basic idea is that there are only two nodes -- one parent and one child.
	// When this channel is for parent, it passes information from child.
	dataChan chan *tDataBundle
}

func (t *testableTask) Init(taskID uint64, framework meritop.Framework) {
	t.id = taskID
	t.framework = framework
	if t.setupLatch != nil {
		t.setupLatch.Done()
	}
}
func (t *testableTask) Exit()                 {}
func (t *testableTask) SetEpoch(epoch uint64) {}

func (t *testableTask) ParentMetaReady(fromID uint64, meta string) {
	if t.dataChan != nil {
		t.dataChan <- &tDataBundle{fromID, meta, "", nil}
	}
}

func (t *testableTask) ChildMetaReady(fromID uint64, meta string) {
	t.ParentMetaReady(fromID, meta)
}

func (t *testableTask) ServeAsParent(fromID uint64, req string) []byte {
	if t.dataChan != nil {
		t.dataChan <- &tDataBundle{fromID, "", req, nil}
	}
	return t.dataMap[req]
}
func (t *testableTask) ServeAsChild(fromID uint64, req string) []byte {
	return t.ServeAsParent(fromID, req)
}
func (t *testableTask) ParentDataReady(fromID uint64, req string, resp []byte) {
	if t.dataChan != nil {
		t.dataChan <- &tDataBundle{fromID, "", req, resp}
	}
}

func (t *testableTask) ChildDataReady(fromID uint64, req string, resp []byte) {
	t.ParentDataReady(fromID, req, resp)
}

func createListener(t *testing.T) net.Listener {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen(\"tcp4\", \"\") failed: %v", err)
	}
	return l
}
