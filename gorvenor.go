package jobqueue

import (
	"sync"
	"time"
)

//OndemandGovernor ondemand governor
func newOndemandGovernor(max, min, maxworker int, jd map[string]JobDescription) governor {
	curr := max
	jc := make(map[string]*int)
	for k := range jd {
		jc[k] = new(int)
	}
	return &OndemandGovernor{
		WorkerCounter:  new(int),
		MaxWorker:      maxworker,
		MaxSleep:       max,
		MinSleep:       min,
		CurSleep:       &curr,
		Locker:         new(sync.Mutex),
		JobCounter:     jc,
		JobDescription: jd,
	}

}

type OndemandGovernor struct {
	MaxSleep       int
	MinSleep       int
	CurSleep       *int
	WorkerCounter  *int
	Locker         *sync.Mutex
	JobCounter     map[string]*int
	MaxWorker      int
	JobDescription map[string]JobDescription
}

func (g OndemandGovernor) AddJob(title string) {
	g.Locker.Lock()
	defer g.Locker.Unlock()
	*g.CurSleep = g.MinSleep
	*g.JobCounter[title] += 1
	*g.WorkerCounter += 1
}

func (g OndemandGovernor) DelJob(title string) {
	g.Locker.Lock()
	defer g.Locker.Unlock()
	*g.JobCounter[title] -= 1
	*g.WorkerCounter -= 1
}

func (g OndemandGovernor) NoJob() {
	t := *g.CurSleep
	t *= 2
	if t > g.MaxSleep {
		t = g.MaxSleep
	}
	*g.CurSleep = t
}

func (g OndemandGovernor) Spawn() (bool, []string) {
	bl := make([]string, 0)
	for k, v := range g.JobDescription {
		if v.Secure {
			g.Locker.Lock()
			if *g.JobCounter[k] >= v.Concurrent {
				bl = append(bl, k)
			}
			g.Locker.Unlock()
		} else {
			if *g.JobCounter[k] >= v.Concurrent {
				bl = append(bl, k)
			}
		}
	}
	time.Sleep(time.Duration(*g.CurSleep) * time.Millisecond)
	if *g.WorkerCounter < g.MaxWorker {
		return true, bl
	}
	g.NoJob()
	return false, bl
}

func (g OndemandGovernor) GetCounter() int {
	return *g.WorkerCounter
}
