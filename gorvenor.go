package jobqueue

import (
	"sync"
	"time"
)

//OndemandGovernor ondemand governor
func NewLocalOndemandGovernor(max, min, maxworker int, jd map[string]JobDescription) Governor {
	curr := max
	jc := make(map[string]*int)
	for k := range jd {
		jc[k] = new(int)
	}
	return &LocalOndemandGovernor{
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

type LocalOndemandGovernor struct {
	MaxSleep       int
	MinSleep       int
	CurSleep       *int
	WorkerCounter  *int
	Locker         *sync.Mutex
	JobCounter     map[string]*int
	MaxWorker      int
	JobDescription map[string]JobDescription
}

func (g LocalOndemandGovernor) AddJob(title string) {
	g.Locker.Lock()
	defer g.Locker.Unlock()
	*g.CurSleep = g.MinSleep
	*g.JobCounter[title] += 1
	*g.WorkerCounter += 1
}

func (g LocalOndemandGovernor) DelJob(title string) {
	g.Locker.Lock()
	defer g.Locker.Unlock()
	*g.JobCounter[title] -= 1
	*g.WorkerCounter -= 1
}

func (g LocalOndemandGovernor) NoJob() {
	g.Locker.Lock()
	defer g.Locker.Unlock()
	t := *g.CurSleep
	t *= 2
	if t > g.MaxSleep {
		t = g.MaxSleep
	}
	*g.CurSleep = t
}

func (g LocalOndemandGovernor) Spawn() (bool, []string) {
	bl := make([]string, 0)
	g.Locker.Lock()
	for k, v := range g.JobDescription {
		if *g.JobCounter[k] >= v.Concurrent {
			bl = append(bl, k)
		}
	}
	sleep := *g.CurSleep
	g.Locker.Unlock()

	time.Sleep(time.Duration(sleep) * time.Millisecond)

	g.Locker.Lock()
	spawn := *g.WorkerCounter < g.MaxWorker
	g.Locker.Unlock()
	if spawn {
		return true, bl
	}
	g.NoJob()
	return false, bl
}

func (g LocalOndemandGovernor) GetCounter() int {
	return *g.WorkerCounter
}
