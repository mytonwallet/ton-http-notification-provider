package jobpool

import (
	"context"
	"fmt"
	"math"
	"time"
)

// worker is a single instance processing jobs.
type worker struct {
	m    *Manager
	jobc <-chan *Job
}

// newWorker creates a new worker. It spins up a new goroutine that waits
// on jobc for new jobs to process.
func newWorker(m *Manager, jobc <-chan *Job) *worker {
	w := &worker{m: m, jobc: jobc}
	go w.run()
	return w
}

// run is the main goroutine in the worker. It listens for new jobs, then
// calls process.
func (w *worker) run() {
	defer w.m.workersWg.Done()
	for job := range w.jobc {
		err := w.process(job)
		if err != nil {
			w.m.logger.Printf("jobqueue: job %v failed: %v", job.ID, err)
		}
	}
}

// process runs a single job.
func (w *worker) process(job *Job) error {
	defer func() {
		w.m.mu.Lock()
		w.m.working[job.Rank]--
		w.m.mu.Unlock()
	}()

	// Find the topic
	w.m.mu.Lock()
	p, found := w.m.tm[job.Topic]
	w.m.mu.Unlock()
	if !found {
		return fmt.Errorf("no processor found for topic %s", job.Topic)
	}

	w.m.testJobStarted() // testing hook

	// Execute the job
	err := p(job)
	if err != nil {
		if job.Retry >= job.MaxRetry {
			// Failed
			w.m.logger.Printf("jobqueue: Job %v failed after %d retries: %v", job.ID, job.Retry+1, err)
			w.m.testJobFailed() // testing hook
			job.State = Failed
			job.Completed = time.Now().UnixNano()
			return w.m.st.Update(context.Background(), job)
		}

		// Retry
		w.m.logger.Printf("jobqueue: Job %v failed on try %d of %d: %v", job.ID, job.Retry+1, job.MaxRetry, err)
		w.m.testJobRetry() // testing hook
		if job.RetryBackoff == "exponential" {
			job.After = time.Now().UnixNano() + job.RetryWait*int64(math.Pow(2, float64(job.Retry-1)))
		} else {
			job.After = time.Now().UnixNano() + job.RetryWait
		}
		job.Priority = -time.Now().Add(w.m.backoff(job.Retry)).UnixNano()
		job.State = Waiting
		job.Retry++
		return w.m.st.Update(context.Background(), job)
	}

	// Successfully executed the job
	job.State = Succeeded
	job.Completed = time.Now().UnixNano()
	err = w.m.st.Update(context.Background(), job)
	if err != nil {
		return err
	}
	w.m.testJobSucceeded()
	return nil
}
