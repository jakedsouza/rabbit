///////////////////////////////////////////////////////////////////////
// Copyright (C) 2016 VMware, Inc. All rights reserved.
// -- VMware Confidential
///////////////////////////////////////////////////////////////////////

package task

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/casualjim/rabbit/eventbus"
	"github.com/go-openapi/strfmt"
	"github.com/pborman/uuid"
)

type Task struct {
	Created  strfmt.DateTime `json:"created,omitempty"`
	ID       TaskID          `json:"id"`
	Type     TaskType        `json:"type,omitempty"`
	Name     string          `json:"name,omitempty"`
	TaskStep Step            `json:"taskstep,omitempty"`

	ctx          context.Context
	log          logrus.FieldLogger
	eventBus     eventbus.EventBus
	eventHandler eventbus.EventHandler
	errorHandler func([]error) error

	successFn func(*Task)
	failFn    func(*Task, error)
}

// State is the state of a task or a step
type State string

const (
	StateNone        State = "none"
	StateCreated     State = "created"
	StateProcessing  State = "processing"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateCanceled    State = "canceled"
	StateRollingback State = "rollingback"
)

// TaskType type of Task
type TaskType string

// TaskID Task Id
type TaskID string

type TaskOpts struct {
	Type      TaskType        `json:"type,omitempty"`
	Ctx       context.Context `json:"-"`
	SuccessFn func(*Task)
	FailFn    func(*Task, error)
	HandlerFn func(eventbus.Event) error
	ErrorFn   func([]error) error
	Log       logrus.FieldLogger
}

func NewTask(taskOpts TaskOpts, step Step) (*Task, error) {
	t := &Task{
		Created:      strfmt.DateTime(time.Now().UTC()),
		ID:           TaskID(uuid.New()),
		Type:         taskOpts.Type,
		TaskStep:     step,
		ctx:          taskOpts.Ctx,
		successFn:    taskOpts.SuccessFn,
		failFn:       taskOpts.FailFn,
		eventBus:     eventbus.New(taskOpts.Log),
		eventHandler: NewEventHandler(taskOpts.HandlerFn),
		errorHandler: NewErrorHandler(taskOpts.ErrorFn),
		log:          Logger(taskOpts.Log),
	}
	t.TaskStep.SetLogger(t.log)
	return t, nil
}

func (t *Task) Run() error {
	var runErr error
	reqCtx, cancel := context.WithCancel(t.ctx)
	t.ctx = reqCtx
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		ctx, err := t.TaskStep.Run(t.ctx, t.eventBus)

		select {
		case <-t.ctx.Done():
			t.log.Debugf("task %s got canceled", t.Name)
			rollbackErr := t.Rollback()
			runErr = t.errorHandler([]error{err, rollbackErr})
			wg.Done()
			return
		default:
			t.ctx = ctx
			runErr = err
		}
		wg.Done()
	}()
	wg.Wait()

	if runErr == nil {
		t.log.Debugf("task %s succeeded", t.Name)
		t.Success()
	} else {
		t.log.Debugf("task %s failed, %s", t.Name, runErr.Error())
		t.Fail(runErr)
	}

	return runErr
}

func (t *Task) Rollback() error {
	return nil
}

//CheckStatus gives the state of the task's step
func (t *Task) CheckStatus() State {
	info := t.TaskStep.GetInfo()
	if info.State == StateNone {
		return StateCreated
	}
	return info.State
}

func (t *Task) GetName() string {
	return t.Name
}

func (t *Task) GetID() string {
	return string(t.ID)
}

func (t *Task) Success() {
	if t.successFn == nil {
		t.eventBus.Close()
		return
	}
	t.successFn(t)
	t.eventBus.Close()
}

func (t *Task) Fail(err error) {
	if t.failFn == nil {
		t.eventBus.Close()
		return
	}
	t.failFn(t, err)
	t.eventBus.Close()
}

//Create a default error handler in case not specified. just put the errors together
func NewErrorHandler(errFn func([]error) error) func(errs []error) error {
	if errFn == nil {
		return func(errs []error) error {
			var err error
			for _, e := range errs {
				if e == nil {
					continue
				}
				if err == nil {
					err = e
					continue
				}
				err = fmt.Errorf("%v.%v", err.Error(), e.Error())
			}
			return err
		}
	}
	return errFn
}

//create a default context handler in case not specified. just return the first context
func NewContextHandler(ctxfn func([]context.Context) context.Context) func([]context.Context) context.Context {
	if ctxfn == nil {
		return func(ctxs []context.Context) context.Context {
			if ctxs == nil {
				return nil
			}
			return ctxs[0]
		}
	}
	return ctxfn
}

//default event handler which does not do anything operation if not defined
func NewEventHandler(evtfn func(eventbus.Event) error) eventbus.EventHandler {
	if evtfn == nil {
		return eventbus.NOOPHandler
	}
	return eventbus.Handler(evtfn)
}

func Logger(log logrus.FieldLogger) logrus.FieldLogger {
	if log == nil {
		return logrus.New()
	}
	return log
}
