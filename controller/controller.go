package controller

import (
	"log"
	"os"
	"strconv"

	"github.com/coreos/go-etcd/etcd"
	"github.com/go-distributed/meritop/pkg/etcdutil"
)

// This is the controller of a job.
// A job needs controller to setup etcd data layout, request
// cluster containers, etc. to setup framework to run.
type Controller struct {
	name           string
	etcdclient     *etcd.Client
	numOfTasks     uint64
	failDetectStop chan bool
	logger         *log.Logger
}

func New(name string, etcd *etcd.Client, numOfTasks uint64) *Controller {
	return &Controller{
		name:       name,
		etcdclient: etcd,
		numOfTasks: numOfTasks,
		logger:     log.New(os.Stdout, "", log.Lshortfile|log.Ltime|log.Ldate),
	}
}

// A controller typical workflow:
// 1. controller sets up etcd layout before any task starts running.
// 2. Being ready, controller lets other tasks to run and reports any failure found.
func (c *Controller) Start() error {
	if err := c.InitEtcdLayout(); err != nil {
		return err
	}
	// Currently no previous changes will be watches before watch is setup.
	// We assumes that ttl is usually a few seconds. watch is setup before that.
	go c.startFailureDetection()
	c.logger.Printf("Controller starting, name: %s, numberOfTask: %d\n", c.name, c.numOfTasks)
	return nil
}

func (c *Controller) Stop() error {
	c.DestroyEtcdLayout()
	c.stopFailureDetection()
	c.logger.Printf("Controller stoping...\n")
	return nil
}

func (c *Controller) InitEtcdLayout() error {
	// Initilize the job epoch to 0
	if _, err := c.etcdclient.Create(etcdutil.EpochPath(c.name), "0", 0); err != nil {
		c.logger.Fatalf("controller create initial epoch failed: %v", err)
	}

	// initiate etcd data layout
	// currently it creates as many unassigned tasks as task masters.
	for i := uint64(0); i < c.numOfTasks; i++ {
		key := etcdutil.FreeTaskPath(c.name, strconv.FormatUint(i, 10))
		if _, err := c.etcdclient.Create(key, "", 0); err != nil {
			c.logger.Fatalf("controller create failed. Key: %s, err: %v", key, err)
		}
	}
	return nil
}

func (c *Controller) DestroyEtcdLayout() error {
	_, err := c.etcdclient.Delete("/", true)
	return err
}

func (c *Controller) startFailureDetection() error {
	c.failDetectStop = make(chan bool, 1)
	return etcdutil.DetectFailure(c.etcdclient, c.name, c.failDetectStop, c.logger)
}

func (c *Controller) stopFailureDetection() error {
	c.failDetectStop <- true
	return nil
}
