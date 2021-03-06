///////////////////////////////////////////////////////////////////////
// Copyright (C) 2016 VMware, Inc. All rights reserved.
// -- VMware Confidential
///////////////////////////////////////////////////////////////////////

package task

import (
	"context"
	"errors"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/casualjim/rabbit/eventbus"
)

type ParalStep struct {
	GenericStep
}

//NewParalStep creates a new parallel step whose substeps can be executed at the same time
//Note that the new Step should be of state StepStateNone, and all of its substeps should be of state StepStateNone too.
func NewParalStep(stepInfo StepInfo,
	log logrus.FieldLogger,
	contextfn func([]context.Context) context.Context,
	errorfn func([]error) error,
	handlerFn func(eventbus.Event) error,
	steps ...Step) *ParalStep {
	//the caller is responsible to make sure stepOpts and all step's state are set to StateNone

	s := &ParalStep{
		GenericStep: GenericStep{
			StepInfo:       stepInfo,
			log:            Logger(log),
			contextHandler: NewContextHandler(contextfn),
			errorHandler:   NewErrorHandler(errorfn),
			eventHandler:   NewEventHandler(handlerFn),
			Steps:          steps},
	}

	for _, step := range steps {
		step.SetLogger(s.log)
	}
	return s
}

func (s *ParalStep) Success(reqCtx context.Context) {
	if s.successFn == nil {
		s.SetState(StateCompleted)
	} else {
		s.successFn(reqCtx, s)
	}
}

func (s *ParalStep) Fail(reqCtx context.Context, err error) {
	if s.failFn == nil {
		s.SetState(StateFailed)
	} else {
		s.failFn(reqCtx, s, err)
	}
}

func (s *ParalStep) Run(reqCtx context.Context, bus eventbus.EventBus) (context.Context, error) {
	s.State = StateProcessing

	bus.Subscribe(s.eventHandler)

	var runError error
	var resultCtx context.Context
	var resultErr error
	var cancelErr error
	ctxc := make(chan context.Context)
	errc := make(chan error)

	var wgCtx sync.WaitGroup
	wgCtx.Add(1)
	go func(reqCtx context.Context) {
		getCtx := false

		ctxs := []context.Context{reqCtx}
		for r := range ctxc {
			ctxs = append(ctxs, r)
			getCtx = true
		}
		if getCtx {
			resultCtx = s.contextHandler(ctxs)
		}
		wgCtx.Done()
	}(reqCtx)

	var wgErr sync.WaitGroup
	wgErr.Add(1)
	go func() {
		var stepErrors []error

		for e := range errc {
			stepErrors = append(stepErrors, e)
		}
		if stepErrors != nil {
			resultErr = s.errorHandler(stepErrors)
		}
		wgErr.Done()
	}()

	go func(ctx context.Context) {
		select {
		case <-reqCtx.Done():
			cancelErr = errors.New("step " + s.Name + " canceled")
			s.log.Debugf("step %s got canceled", s.Name)
		}

	}(reqCtx)

	var wgCancel sync.WaitGroup
	wgCancel.Add(len(s.Steps))
	for _, step := range s.Steps {
		ctx := reqCtx
		go func(step Step, ctx context.Context) {
			ctx, err := step.Run(ctx, bus)
			if err != nil {
				errc <- err
			} else {
				ctxc <- ctx
			}
			wgCancel.Done()
		}(step, ctx)
	}

	wgCancel.Wait()
	close(ctxc)
	close(errc)

	wgCtx.Wait()
	wgErr.Wait()

	var errs []error
	if cancelErr != nil {
		if resultErr != nil {
			errs = append(errs, resultErr)
		}
		errs = append(errs, cancelErr)

		_, rollbackError := s.Rollback(reqCtx, bus)

		if rollbackError != nil {
			errs = append(errs, rollbackError)
		}
		runError = s.errorHandler(errs)

		s.log.Debugf("step %s canceled. %s", s.Name, runError)
		return reqCtx, runError

	} else if resultErr != nil {
		errs = append(errs, resultErr)
		runError = s.errorHandler(errs)
		s.Fail(reqCtx, runError)

		s.log.Debugf("step %s failed, %s", s.Name, runError.Error())
		return reqCtx, runError

	} else if resultCtx != nil {
		s.Success(resultCtx)
		return resultCtx, nil
	}

	return reqCtx, nil
}
