// Copyright © 2021 Kaleido, Inc.
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

package events

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/kaleido-io/firefly/internal/config"
	"github.com/kaleido-io/firefly/internal/i18n"
	"github.com/kaleido-io/firefly/internal/log"
	"github.com/kaleido-io/firefly/internal/retry"
	"github.com/kaleido-io/firefly/pkg/database"
	"github.com/kaleido-io/firefly/pkg/fftypes"
)

type ackNack struct {
	isNack bool
	offset int64
}

type eventDispatcher struct {
	ctx          context.Context
	database     database.Plugin
	connID       string
	subscription *subscription
	eventPoller  *eventPoller
	cancelCtx    func()
	mux          sync.Mutex
	inflight     map[uuid.UUID]*fftypes.Event
	namespace    string
	readAhead    int
	acksNacks    chan ackNack
}

func newEventDispatcher(ctx context.Context, di database.Plugin, connID string, sub *subscription) *eventDispatcher {
	ctx, cancelCtx := context.WithCancel(ctx)
	ed := &eventDispatcher{
		ctx: log.WithLogField(log.WithLogField(ctx,
			"role", fmt.Sprintf("ed[%s]", connID)),
			"sub", fmt.Sprintf("%s/%s:%s", sub.definition.ID, sub.definition.Namespace, sub.definition.Name)),
		database:     di,
		connID:       connID,
		cancelCtx:    cancelCtx,
		subscription: sub,
		namespace:    sub.definition.Namespace,
		readAhead:    int(config.GetUint(config.SubscriptionDefaultsReadAhead)),
		acksNacks:    make(chan ackNack),
	}

	pollerConf := eventPollerConf{
		limitNamespace:             sub.definition.Namespace,
		eventBatchSize:             config.GetInt(config.EventDispatcherBufferLength),
		eventBatchTimeout:          config.GetDuration(config.EventDispatcherBatchTimeout),
		eventPollTimeout:           config.GetDuration(config.EventDispatcherPollTimeout),
		startupOffsetRetryAttempts: config.GetInt(config.OrchestratorStartupAttempts),
		retry: retry.Retry{
			InitialDelay: config.GetDuration(config.EventDispatcherRetryInitDelay),
			MaximumDelay: config.GetDuration(config.EventDispatcherRetryMaxDelay),
			Factor:       config.GetFloat64(config.EventDispatcherRetryFactor),
		},
		offsetType:       fftypes.OffsetTypeSubscription,
		offsetNamespace:  sub.definition.Namespace,
		offsetName:       sub.definition.Name,
		newEventsHandler: ed.bufferedDelivery,
		ephemeral:        sub.definition.Ephemeral,
	}
	if sub.definition.Options.ReadAhead != nil {
		ed.readAhead = int(*sub.definition.Options.ReadAhead)
	}

	ed.eventPoller = newEventPoller(ctx, di, pollerConf)
	return ed
}

func (ed *eventDispatcher) start() {
	go ed.electAndStart()
}

func (ed *eventDispatcher) electAndStart() {
	l := log.L(ed.ctx)
	l.Debugf("Dispatcher attempting to become leader")
	select {
	case ed.subscription.dispatcherElection <- true:
		l.Debugf("Dispatcher became leader")
	case <-ed.eventPoller.closed:
		l.Debugf("Closed before we became leader")
		return
	}
	// Wait until we close
	<-ed.eventPoller.closed
	// Unelect ourselves on close, to let another dispatcher in
	<-ed.subscription.dispatcherElection
}

func (ed *eventDispatcher) enrichEvents(events []*fftypes.Event) ([]*fftypes.EventDelivery, error) {
	// We need all the messages that match event references
	refIds := make([]driver.Value, len(events))
	for i, e := range events {
		if e.Reference != nil {
			refIds[i] = *e.Reference
		}
	}

	mfb := database.MessageQueryFactory.NewFilter(ed.ctx)
	msgFilter := mfb.And(
		mfb.In("id", refIds),
		mfb.Eq("namespace", ed.namespace),
	)
	msgs, err := ed.database.GetMessages(ed.ctx, msgFilter)
	if err != nil {
		return nil, err
	}

	dfb := database.DataQueryFactory.NewFilter(ed.ctx)
	dataFilter := dfb.And(
		dfb.In("id", refIds),
		dfb.Eq("namespace", ed.namespace),
	)
	dataRefs, err := ed.database.GetDataRefs(ed.ctx, dataFilter)
	if err != nil {
		return nil, err
	}

	enriched := make([]*fftypes.EventDelivery, len(events))
	for i, e := range events {
		enriched[i] = &fftypes.EventDelivery{
			Event:        *e,
			Subscription: ed.subscription.definition.SubscriptionRef,
		}
		for _, msg := range msgs {
			if *e.Reference == *msg.Header.ID {
				enriched[i].Message = msg
				break
			}
		}
		for _, dr := range dataRefs {
			if *e.Reference == *dr.ID {
				enriched[i].Data = &dr
				break
			}
		}
	}

	return enriched, nil

}

func (ed *eventDispatcher) filterEvents(candidates []*fftypes.EventDelivery) []*fftypes.EventDelivery {
	matchingEvents := make([]*fftypes.EventDelivery, 0, len(candidates))
	for _, event := range candidates {
		filter := ed.subscription
		if filter.eventMatcher != nil && !filter.eventMatcher.MatchString(string(event.Type)) {
			continue
		}
		msg := event.Message
		topic := ""
		group := ""
		context := ""
		if msg != nil {
			topic = msg.Header.Topic
			context = msg.Header.Context
		}
		if filter.topicFilter != nil && !filter.topicFilter.MatchString(topic) {
			continue
		}
		if filter.contextFilter != nil && !filter.contextFilter.MatchString(context) {
			continue
		}
		if filter.groupFilter != nil && !filter.groupFilter.MatchString(group) {
			continue
		}
		matchingEvents = append(matchingEvents, event)
	}
	return matchingEvents
}

func (ed *eventDispatcher) bufferedDelivery(events []*fftypes.Event) (bool, error) {
	// At this point, the page of messages we've been given are loaded from the DB into memory,
	// but we can only make them in-flight and push them to the client up to the maximum
	// readahead (which is likely lower than our page size - 1 by default)

	if len(events) == 0 {
		return false, nil
	}

	l := log.L(ed.ctx)
	candidates, err := ed.enrichEvents(events)
	if err != nil {
		return false, err
	}

	matching := ed.filterEvents(candidates)
	matchCount := len(matching)
	dispatched := 0

	// We stay here blocked until we've consumed all the messages in the buffer,
	// or a reset event happens
	for matchCount > 0 {
		ed.mux.Lock()
		var disapatchable []*fftypes.EventDelivery
		inflightCount := len(ed.inflight)
		dispatchCount := 1 + ed.readAhead - inflightCount
		if dispatchCount == len(matching) {
			disapatchable = matching
		} else if dispatchCount > 0 {
			disapatchable = matching[0:dispatchCount]
			matching = matching[dispatchCount:]
		}
		ed.mux.Unlock()

		l.Debugf("Dispatcher event state: candidates=%d matched=%d inflight=%d queued=%d dispatched=%d dispatchable=%d",
			len(candidates), matchCount, inflightCount, len(matching), dispatched, len(disapatchable))
		for _, event := range disapatchable {
			err := ed.deliverEvent(event)
			if err != nil {
				return false, err
			}
			dispatched++
		}

		// Block until we're closed, or woken due to a delivery response
		select {
		case <-ed.ctx.Done():
			return false, i18n.NewError(ed.ctx, i18n.MsgDispatcherClosing)
		case an := <-ed.acksNacks:
			if an.isNack {
				ed.mux.Lock()
				// If we're rejected, we need to redeliver all messages from this offset onwards,
				// even if we've delivered messages after that.
				// That means resetting the polling offest, and clearing out all our state
				if ed.eventPoller.pollingOffset > an.offset {
					ed.eventPoller.rewindPollingOffset(an.offset)
				}
				ed.inflight = map[uuid.UUID]*fftypes.Event{}
				ed.mux.Unlock()
			}
		}
	}
	return false, nil
}

func (ed *eventDispatcher) deliverEvent(event *fftypes.EventDelivery) error {
	l := log.L(ed.ctx)
	l.Debugf("Dispatching event: %.10d/%s [%s]: ref=%s/%s", event.Sequence, event.ID, event.Type, event.Namespace, event.Reference)
	return nil
}

func (ed *eventDispatcher) deliveryResponse(response *fftypes.EventDeliveryResponse) error {
	l := log.L(ed.ctx)

	ed.mux.Lock()
	var an ackNack
	event, found := ed.inflight[*response.ID]
	if found {
		delete(ed.inflight, *response.ID)
		an.offset = event.Sequence
		an.isNack = response.Rejected
	}
	defer ed.mux.Unlock()

	// Do some extra logging and persistent actions now we're out of lock
	if !found {
		l.Warnf("Response for event not in flight: %s rejected=%t info='%s' (likely previous reject)", response.ID, response.Rejected, response.Info)
		return nil
	}

	l.Debugf("Response for event: %.10d/%s [%s]: ref=%s/%s rejected=%t info='%s'", event.Sequence, event.ID, event.Type, event.Namespace, event.Reference, response.Rejected, response.Info)
	// We don't do any meaningful work in this call, we just set things up so the right thing
	// will happen when the poller wakes up. So we need to pass it over
	select {
	case ed.acksNacks <- an:
	case <-ed.ctx.Done():
		return i18n.NewError(ed.ctx, i18n.MsgDispatcherClosing)
	}

	return nil
}

func (ed *eventDispatcher) close() {
	ed.cancelCtx()
	<-ed.eventPoller.closed
}