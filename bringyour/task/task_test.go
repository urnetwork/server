package task

import (
	"context"
    "testing"
    "fmt"
    "sync"
    "time"
    mathrand "math/rand"
    "errors"

    "github.com/go-playground/assert/v2"

    // "bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour"
)


type Work1Args struct {
}

type Work1Result struct {
}


func Work1(
	work1 *Work1Args,
	clientSession *session.ClientSession,
) (*Work1Result, error) {
	if 0 == mathrand.Intn(3) {
		return nil, errors.New("Error.")
	}
	return &Work1Result{}, nil
}

func Work1Post(
	work1 *Work1Args,
	work1Result *Work1Result,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	if 0 == mathrand.Intn(3) {
		return errors.New("Post error.")
	}
	return nil
}


func TestTask(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	RescheduleTimeout = 1 * time.Second

	ctx := context.Background()

	n := 10
	m := 10000
	k := 100


	targetRunCount := 2 * m + k


	stateLock := sync.Mutex{}
	runCounts := map[bringyour.Id]int{}
	postRescheduledRunCounts := map[bringyour.Id]int{}
	workerCount := 0

	clientSession := session.Testing_CreateClientSession(ctx, nil)
	defer clientSession.Cancel()

	for i := 0; i < m; i += 1 {
		ScheduleTask(
			Work1,
			&Work1Args{},
			clientSession,
			RunOnce("unique", i % k),
		)
	}


	for i := 0; i < n; i += 1 {
		taskWorker := NewTaskWorker(ctx)
		taskWorker.AddTargets(NewTaskTargetWithPost(Work1, Work1Post))

		go func() {
			stateLock.Lock()
			workerCount += 1
			stateLock.Unlock()
			defer func() {
				stateLock.Lock()
				workerCount -= 1
				stateLock.Unlock()
			}()
			
			for {
				select {
				case <- clientSession.Ctx.Done():
					return
				default:
				}

				finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := taskWorker.EvalTasks(10)
				if err != nil {
					panic(err)
				}
				stateLock.Lock()
				for _, taskId := range finishedTaskIds {
					runCounts[taskId] += 1
				}
				for _, taskId := range postRescheduledTaskIds {
					postRescheduledRunCounts[taskId] += 1
				}
				stateLock.Unlock()
				

				if 0 == len(finishedTaskIds) + len(rescheduledTaskIds) + len(postRescheduledTaskIds) {
					select {
					case <- clientSession.Ctx.Done():
						return
					case <- time.After(1 * time.Second):
					}
				}
			}
		}()
	}


	for i := 0; i < m; i += 1 {
		ScheduleTask(
			Work1,
			&Work1Args{},
			clientSession,
		)
	}
	for i := 0; i < m; i += 1 {
		ScheduleTask(
			Work1,
			&Work1Args{},
			clientSession,
			RunOnce("task", i),
		)
	}


	WaitWork:
	for {
		stateLock.Lock()
		netRunCount := 0
		for _, runCount := range runCounts {
			netRunCount += runCount
		}
		pendingTaskIds := ListPendingTasks(ctx)
		rescheduledTaskIds := ListRescheduledTasks(ctx)
		claimedTaskIds := ListClaimedTasks(ctx)
		finishedTaskIds := ListFinishedTasks(ctx)
		fmt.Printf("Tasks pending=%d (rescheduled=%d, claimed=%d, finished=%d)\n", len(pendingTaskIds), len(rescheduledTaskIds), len(claimedTaskIds), len(finishedTaskIds))
		finished := (len(pendingTaskIds) == 0)
		stateLock.Unlock()

		if finished {
			clientSession.Cancel()
			break
		}

		select {
		case <- clientSession.Ctx.Done():
			break WaitWork
		case <- time.After(1 * time.Second):
		}
	}

	WaitDrain:
	for {
		stateLock.Lock()
		finished := (workerCount == 0)
		stateLock.Unlock()

		if finished {
			break
		}

		select {
		case <- ctx.Done():
			break WaitDrain
		case <- time.After(1 * time.Second):
		}
	}


	netRunCount := 0
	for _, runCount := range runCounts {
		netRunCount += runCount
	}
	assert.Equal(t, netRunCount, targetRunCount)


	netTaskCount := 0
	for _, runCount := range runCounts {
		netTaskCount += runCount
	}
	for _, runCount := range postRescheduledRunCounts {
		netTaskCount += runCount
	}

	removedCount := RemoveFinishedTasks(ctx, bringyour.NowUtc())
	assert.Equal(t, int(removedCount), netTaskCount)
	assert.Equal(t, 0, len(ListFinishedTasks(ctx)))
})}
