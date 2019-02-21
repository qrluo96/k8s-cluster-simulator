package kubesim

import (
	"context"
	"fmt"
	"time"

	"github.com/cpuguy83/strongerrors"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/core"
	"k8s.io/kubernetes/pkg/scheduler/nodeinfo"

	"github.com/ordovicia/kubernetes-simulator/api"
	"github.com/ordovicia/kubernetes-simulator/kubesim/clock"
	"github.com/ordovicia/kubernetes-simulator/kubesim/config"
	"github.com/ordovicia/kubernetes-simulator/kubesim/node"
	"github.com/ordovicia/kubernetes-simulator/kubesim/queue"
	"github.com/ordovicia/kubernetes-simulator/kubesim/scheduler"
	"github.com/ordovicia/kubernetes-simulator/log"
)

// KubeSim represents a kubernetes cluster simulator.
type KubeSim struct {
	nodes    map[string]*node.Node
	podQueue queue.Queue

	tick  int
	clock clock.Clock

	submitters []api.Submitter
	scheduler  scheduler.Scheduler
}

// NewKubeSim creates a new KubeSim with the config.
func NewKubeSim(conf *config.Config) (*KubeSim, error) {
	log.G(context.TODO()).Debugf("Config: %+v", *conf)

	if err := configLog(conf.LogLevel); err != nil {
		return nil, errors.Errorf("error configuring: %s", err.Error())
	}

	clk := time.Now()
	if conf.StartClock != "" {
		var err error
		clk, err = time.Parse(time.RFC3339, conf.StartClock)
		if err != nil {
			return nil, err
		}
	}

	nodes := map[string]*node.Node{}
	for _, nodeConf := range conf.Cluster.Nodes {
		log.L.Debugf("NodeConfig: %+v", nodeConf)

		nodeV1, err := config.BuildNode(nodeConf, conf.StartClock)
		if err != nil {
			return nil, errors.Errorf("error building node config: %s", err.Error())
		}

		n := node.NewNode(nodeV1)
		nodes[nodeV1.Name] = &n

		log.L.Debugf("Node %q created", nodeV1.Name)
	}

	kubesim := KubeSim{
		nodes:     nodes,
		podQueue:  queue.NewPriorityQueue(),
		tick:      conf.Tick,
		clock:     clock.NewClock(clk),
		scheduler: scheduler.NewScheduler(),
	}

	return &kubesim, nil
}

// NewKubeSimFromConfigPath creates a new KubeSim with config from confPath (excluding file path).
func NewKubeSimFromConfigPath(confPath string) (*KubeSim, error) {
	conf, err := readConfig(confPath)
	if err != nil {
		return nil, errors.Errorf("error reading config: %s", err.Error())
	}

	return NewKubeSim(conf)
}

// AddSubmitter adds a new submitter plugin to this KubeSim.
func (k *KubeSim) AddSubmitter(submitter api.Submitter) {
	k.submitters = append(k.submitters, submitter)
}

// Scheduler retuns *scheduler.Scheduler of this Kubesim
func (k *KubeSim) Scheduler() *scheduler.Scheduler {
	return &k.scheduler
}

// Run executes the main loop, which invokes scheduler plugins and binds pods to the selected nodes.
func (k *KubeSim) Run(ctx context.Context) error {
	tick := make(chan clock.Clock, 1)
	go func() {
		for {
			tick <- k.clock
			k.clock = k.clock.Add(time.Duration(k.tick) * time.Second)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case clock := <-tick:
			log.L.Debugf("Clock %s", clock.String())

			nodes, _ := k.List()
			if err := k.submit(clock, nodes); err != nil {
				return err
			}

			pod, err := k.podQueue.Pop()
			if err == queue.ErrEmptyQueue {
				continue
			}

			err = k.scheduleOne(clock, pod)
			if fitErr, ok := err.(*errPodDoesNotFit); ok {
				log.L.Debug(fitErr.Error())
				k.podQueue.PlaceBack(pod)
				continue
			}

			if err != nil {
				return err
			}
		}
	}
}

// List implements "k8s.io/pkg/scheduler/algorithm".NodeLister
func (k *KubeSim) List() ([]*v1.Node, error) {
	nodes := make([]*v1.Node, 0, len(k.nodes))
	for _, node := range k.nodes {
		nodes = append(nodes, node.ToV1())
	}
	return nodes, nil
}

func (k *KubeSim) submit(clock clock.Clock, nodes []*v1.Node) error {
	for _, submitter := range k.submitters {
		pods, err := submitter.Submit(clock, nodes)
		if err != nil {
			return err
		}

		for _, pod := range pods {
			pod.CreationTimestamp = clock.ToMetaV1()

			log.L.Tracef("Submit %v", pod)
			log.L.Debugf("Submit %q", pod.Name)

			k.podQueue.Push(pod)
		}
	}

	return nil
}

type errPodDoesNotFit struct {
	pod *v1.Pod
}

func (e *errPodDoesNotFit) Error() string {
	return fmt.Sprintf("Pod %q does not fit in any node", e.pod.Name)
}

func (k *KubeSim) scheduleOne(clock clock.Clock, pod *v1.Pod) error {
	log.L.Tracef("Trying to schedule pod %v", pod)
	log.L.Debugf("Trying to schedule pod %q", pod.Name)

	nodeInfoMap := map[string]*nodeinfo.NodeInfo{}
	for name, node := range k.nodes {
		nodeInfoMap[name] = node.ToNodeInfo(clock)
	}

	result, err := k.scheduler.Schedule(pod, k, nodeInfoMap)

	if err != nil {
		if _, ok := err.(*core.FitError); ok {
			return &errPodDoesNotFit{pod}
		}
		return err
	}

	nodeName := result.SuggestedHost
	log.L.Debugf("Selected node %q", nodeName)

	node, ok := k.nodes[nodeName]
	if !ok {
		return errors.Errorf("No node named %q", nodeName)
	}

	if err := node.CreatePod(clock, pod); err != nil {
		return err
	}

	return nil
}

// readConfig reads and parses a config from the path (excluding file extension).
func readConfig(path string) (*config.Config, error) {
	viper.SetConfigName(path)
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	log.G(context.TODO()).Debugf("Using config file %s", viper.ConfigFileUsed())

	var conf = config.Config{
		LogLevel:   "info",
		Tick:       10,
		StartClock: "",
		// APIPort:     10250,
		// MetricsPort: 10255,
		Cluster: config.ClusterConfig{Nodes: []config.NodeConfig{}},
	}

	if err := viper.Unmarshal(&conf); err != nil {
		return nil, err
	}

	return &conf, nil
}

func configLog(logLevel string) error {
	level, err := log.ParseLevel(logLevel)
	if err != nil {
		return strongerrors.InvalidArgument(errors.Errorf("%s: log level %q not supported", err.Error(), level))
	}
	logrus.SetLevel(level)

	logger := log.L
	log.L = logger

	return nil
}
