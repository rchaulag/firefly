// Copyright © 2021 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
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

package syncasync

import (
	"context"
	"fmt"
	"testing"

	"github.com/hyperledger-labs/firefly/mocks/databasemocks"
	"github.com/hyperledger-labs/firefly/mocks/datamocks"
	"github.com/hyperledger-labs/firefly/mocks/eventmocks"
	"github.com/hyperledger-labs/firefly/mocks/privatemessagingmocks"
	"github.com/hyperledger-labs/firefly/pkg/fftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func newTestSyncAsyncBridge(t *testing.T) (*syncAsyncBridge, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	mei := &eventmocks.EventManager{}
	mpm := &privatemessagingmocks.Manager{}
	sa := NewSyncAsyncBridge(ctx, mdi, mdm, mei, mpm)
	return sa.(*syncAsyncBridge), cancel
}

func TestRequestReplyOk(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	var requestID *fftypes.UUID
	replyID := fftypes.NewUUID()
	dataID := fftypes.NewUUID()

	mei := sa.events.(*eventmocks.EventManager)
	mei.On("AddSystemEventListener", "ns1", mock.Anything).Return(nil)

	mpm := sa.messaging.(*privatemessagingmocks.Manager)
	send := mpm.On("SendMessageWithID", sa.ctx, "ns1", mock.Anything)
	send.RunFn = func(a mock.Arguments) {
		msg := a[2].(*fftypes.MessageInOut)
		assert.NotNil(t, msg.Header.ID)
		requestID = msg.Header.ID
		assert.Equal(t, "mytag", msg.Header.Tag)
		send.ReturnArguments = mock.Arguments{&msg.Message, nil}

		go func() {
			sa.eventCallback(&fftypes.EventDelivery{
				Event: fftypes.Event{
					ID:        fftypes.NewUUID(),
					Type:      fftypes.EventTypeMessageConfirmed,
					Reference: replyID,
					Namespace: "ns1",
				},
			})
		}()
	}

	mdi := sa.database.(*databasemocks.Plugin)
	gmid := mdi.On("GetMessageByID", sa.ctx, mock.Anything)
	gmid.RunFn = func(a mock.Arguments) {
		assert.NotNil(t, requestID)
		gmid.ReturnArguments = mock.Arguments{
			&fftypes.Message{
				Header: fftypes.MessageHeader{
					ID:  replyID,
					CID: requestID,
				},
				Data: fftypes.DataRefs{
					{ID: dataID},
				},
			}, nil,
		}
	}

	mdm := sa.data.(*datamocks.Manager)
	mdm.On("GetMessageData", sa.ctx, mock.Anything, true).Return([]*fftypes.Data{
		{ID: dataID, Value: fftypes.Byteable(`"response data"`)},
	}, true, nil)

	reply, err := sa.RequestReply(sa.ctx, "ns1", &fftypes.MessageInOut{
		Message: fftypes.Message{
			Header: fftypes.MessageHeader{
				Tag: "mytag",
			},
		},
	})
	assert.NoError(t, err)
	assert.Equal(t, *replyID, *reply.Header.ID)
	assert.Equal(t, `"response data"`, string(reply.InlineData[0].Value))

}

func TestRequestReplyTimeout(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	cancel()

	mei := sa.events.(*eventmocks.EventManager)
	mei.On("AddSystemEventListener", "ns1", mock.Anything).Return(nil)

	mpm := sa.messaging.(*privatemessagingmocks.Manager)
	mpm.On("SendMessageWithID", sa.ctx, "ns1", mock.Anything).Return(&fftypes.Message{}, nil)

	_, err := sa.RequestReply(sa.ctx, "ns1", &fftypes.MessageInOut{
		Message: fftypes.Message{
			Header: fftypes.MessageHeader{
				Tag: "mytag",
			},
		},
	})
	assert.Regexp(t, "FF10260", err)

}

func TestRequestReplySendFail(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	mei := sa.events.(*eventmocks.EventManager)
	mei.On("AddSystemEventListener", "ns1", mock.Anything).Return(nil)

	mpm := sa.messaging.(*privatemessagingmocks.Manager)
	mpm.On("SendMessageWithID", sa.ctx, "ns1", mock.Anything).Return(nil, fmt.Errorf("pop"))

	_, err := sa.RequestReply(sa.ctx, "ns1", &fftypes.MessageInOut{
		Message: fftypes.Message{
			Header: fftypes.MessageHeader{
				Tag: "mytag",
			},
		},
	})
	assert.Regexp(t, "pop", err)

}

func TestRequestSetupSystemListenerFail(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	mei := sa.events.(*eventmocks.EventManager)
	mei.On("AddSystemEventListener", "ns1", mock.Anything).Return(fmt.Errorf("pop"))

	_, err := sa.RequestReply(sa.ctx, "ns1", &fftypes.MessageInOut{
		Message: fftypes.Message{
			Header: fftypes.MessageHeader{
				Tag: "mytag",
			},
		},
	})
	assert.Regexp(t, "pop", err)

}

func TestRequestSetupSystemMissingTag(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	_, err := sa.RequestReply(sa.ctx, "ns1", &fftypes.MessageInOut{})
	assert.Regexp(t, "FF10261", err)

}

func TestRequestSetupSystemInvalidCID(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	_, err := sa.RequestReply(sa.ctx, "ns1", &fftypes.MessageInOut{
		Message: fftypes.Message{
			Header: fftypes.MessageHeader{
				Tag: "mytag",
				CID: fftypes.NewUUID(),
			},
		},
	})
	assert.Regexp(t, "FF10262", err)

}

func TestEventCallbackNotInflight(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	err := sa.eventCallback(&fftypes.EventDelivery{
		Event: fftypes.Event{
			Namespace: "ns1",
			ID:        fftypes.NewUUID(),
			Reference: fftypes.NewUUID(),
			Type:      fftypes.EventTypeMessageConfirmed,
		},
	})
	assert.NoError(t, err)

}

func TestEventCallbackWrongType(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	responseID := fftypes.NewUUID()
	sa.inflight = map[string]map[fftypes.UUID]*inflightRequest{
		"ns1": {
			*responseID: &inflightRequest{},
		},
	}

	err := sa.eventCallback(&fftypes.EventDelivery{
		Event: fftypes.Event{
			Namespace: "ns1",
			ID:        fftypes.NewUUID(),
			Reference: fftypes.NewUUID(),
			Type:      fftypes.EventTypeMessageRejected,
		},
	})
	assert.NoError(t, err)

}

func TestEventCallbackMsgLookupFail(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	responseID := fftypes.NewUUID()
	sa.inflight = map[string]map[fftypes.UUID]*inflightRequest{
		"ns1": {
			*responseID: &inflightRequest{},
		},
	}

	mdi := sa.database.(*databasemocks.Plugin)
	mdi.On("GetMessageByID", sa.ctx, mock.Anything).Return(nil, fmt.Errorf("pop"))

	err := sa.eventCallback(&fftypes.EventDelivery{
		Event: fftypes.Event{
			Namespace: "ns1",
			ID:        fftypes.NewUUID(),
			Reference: fftypes.NewUUID(),
			Type:      fftypes.EventTypeMessageConfirmed,
		},
	})
	assert.EqualError(t, err, "pop")

}

func TestEventCallbackMsgNotFound(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	responseID := fftypes.NewUUID()
	sa.inflight = map[string]map[fftypes.UUID]*inflightRequest{
		"ns1": {
			*responseID: &inflightRequest{},
		},
	}

	mdi := sa.database.(*databasemocks.Plugin)
	mdi.On("GetMessageByID", sa.ctx, mock.Anything).Return(nil, nil)

	err := sa.eventCallback(&fftypes.EventDelivery{
		Event: fftypes.Event{
			Namespace: "ns1",
			ID:        fftypes.NewUUID(),
			Reference: fftypes.NewUUID(),
			Type:      fftypes.EventTypeMessageConfirmed,
		},
	})
	assert.NoError(t, err)

	mdi.AssertExpectations(t)
}

func TestEventCallbackMsgDataLookupFail(t *testing.T) {

	sa, cancel := newTestSyncAsyncBridge(t)
	defer cancel()

	mdm := sa.data.(*datamocks.Manager)
	mdm.On("GetMessageData", sa.ctx, mock.Anything, true).Return(nil, false, fmt.Errorf("pop"))

	sa.resolveInflight(&inflightRequest{}, &fftypes.Message{
		Header: fftypes.MessageHeader{
			ID:  fftypes.NewUUID(),
			CID: fftypes.NewUUID(),
		},
	})

	mdm.AssertExpectations(t)
}