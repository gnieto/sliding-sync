package sync3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/matrix-org/sync-v3/testutils"
)

func newSortableRoom(roomID string, lastMsgTimestamp int64) SortableRoom {
	return SortableRoom{
		RoomID:               roomID,
		Name:                 "Room " + roomID,
		LastMessageTimestamp: lastMsgTimestamp,
		LastEventJSON: json.RawMessage(
			fmt.Sprintf(`{"type":"m.room.message","content":{"body":"hello"},"origin_server_ts":%d}`, lastMsgTimestamp),
		),
	}
}

func mockLazyRoomOverride(loadPos int64, roomIDs []string, maxTimelineEvents int) map[string]UserRoomData {
	result := make(map[string]UserRoomData)
	for _, roomID := range roomIDs {
		result[roomID] = UserRoomData{
			Timeline: []json.RawMessage{
				[]byte(`{}`),
			},
		}
	}
	return result
}

// Sync an account with 3 rooms and check that we can grab all rooms and they are sorted correctly initially. Checks
// that basic UPDATE and DELETE/INSERT works when tracking all rooms.
func TestConnStateInitial(t *testing.T) {
	connID := ConnID{
		SessionID: "s",
		DeviceID:  "d",
	}
	userID := "@TestConnStateInitial_alice:localhost"
	timestampNow := int64(1632131678061)
	// initial sort order B, C, A
	roomA := newSortableRoom("!a:localhost", timestampNow-8000)
	roomB := newSortableRoom("!b:localhost", timestampNow)
	roomC := newSortableRoom("!c:localhost", timestampNow-4000)
	globalCache := NewGlobalCache(nil)
	globalCache.AssignRoom(roomA)
	globalCache.AssignRoom(roomB)
	globalCache.AssignRoom(roomC)
	globalCache.jrt.UserJoinedRoom(userID, roomA.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomB.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomC.RoomID)
	globalCache.LoadJoinedRoomsOverride = func(userID string) (pos int64, joinedRooms []SortableRoom, err error) {
		return 1, []SortableRoom{
			roomA, roomB, roomC,
		}, nil
	}
	userCache := NewUserCache(userID, nil)
	userCache.LazyRoomDataOverride = func(loadPos int64, roomIDs []string, maxTimelineEvents int) map[string]UserRoomData {
		result := make(map[string]UserRoomData)
		for _, roomID := range roomIDs {
			result[roomID] = UserRoomData{
				Timeline: []json.RawMessage{
					globalCache.LoadRoom(roomID).LastEventJSON,
				},
			}
		}
		return result
	}
	cs := NewConnState(userID, userCache, globalCache)
	if userID != cs.UserID() {
		t.Fatalf("UserID returned wrong value, got %v want %v", cs.UserID(), userID)
	}
	res, err := cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 9},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, false, res, &Response{
		Count: 3,
		Ops: []ResponseOp{
			&ResponseOpRange{
				Operation: "SYNC",
				Range:     []int64{0, 9},
				Rooms: []Room{
					{
						RoomID:   roomB.RoomID,
						Name:     roomB.Name,
						Timeline: []json.RawMessage{roomB.LastEventJSON},
					},
					{
						RoomID:   roomC.RoomID,
						Name:     roomC.Name,
						Timeline: []json.RawMessage{roomC.LastEventJSON},
					},
					{
						RoomID:   roomA.RoomID,
						Name:     roomA.Name,
						Timeline: []json.RawMessage{roomA.LastEventJSON},
					},
				},
			},
		},
	})

	// bump A to the top
	globalCache.OnNewEvents(roomA.RoomID, []json.RawMessage{
		testutils.NewEvent(t, "unimportant", "me", struct{}{}, timestampNow+1000),
	}, 1)

	// request again for the diff
	res, err = cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 9},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, true, res, &Response{
		Count: 3,
		Ops: []ResponseOp{
			&ResponseOpSingle{
				Operation: "DELETE",
				Index:     intPtr(2),
			},
			&ResponseOpSingle{
				Operation: "INSERT",
				Index:     intPtr(0),
				Room: &Room{
					RoomID: roomA.RoomID,
				},
			},
		},
	})

	// another message should just update
	globalCache.OnNewEvents(roomA.RoomID, []json.RawMessage{
		testutils.NewEvent(t, "unimportant", "me", struct{}{}, timestampNow+2000),
	}, 1)
	res, err = cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 9},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, true, res, &Response{
		Count: 3,
		Ops: []ResponseOp{
			&ResponseOpSingle{
				Operation: "UPDATE",
				Index:     intPtr(0),
				Room: &Room{
					RoomID: roomA.RoomID,
				},
			},
		},
	})
}

// Test that multiple ranges can be tracked in a single request
func TestConnStateMultipleRanges(t *testing.T) {
	connID := ConnID{
		SessionID: "s",
		DeviceID:  "d",
	}
	userID := "@TestConnStateMultipleRanges_alice:localhost"
	timestampNow := int64(1632131678061)
	var rooms []SortableRoom
	var roomIDs []string
	globalCache := NewGlobalCache(nil)
	roomIDToRoom := make(map[string]SortableRoom)
	for i := 0; i < 10; i++ {
		roomID := fmt.Sprintf("!%d:localhost", i)
		room := SortableRoom{
			RoomID: roomID,
			Name:   fmt.Sprintf("Room %d", i),
			// room 1 is most recent, 10 is least recent
			LastMessageTimestamp: timestampNow - int64(i*1000),
			LastEventJSON:        []byte(`{}`),
		}
		rooms = append(rooms, room)
		roomIDs = append(roomIDs, roomID)
		roomIDToRoom[roomID] = room
		globalCache.AssignRoom(room)
		globalCache.jrt.UserJoinedRoom(userID, roomID)
	}
	globalCache.LoadJoinedRoomsOverride = func(userID string) (pos int64, joinedRooms []SortableRoom, err error) {
		return 1, rooms, nil
	}
	userCache := NewUserCache(userID, nil)
	userCache.LazyRoomDataOverride = mockLazyRoomOverride
	cs := NewConnState(userID, userCache, globalCache)

	// request first page
	res, err := cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 2},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, true, res, &Response{
		Count: int64(len(rooms)),
		Ops: []ResponseOp{
			&ResponseOpRange{
				Operation: "SYNC",
				Range:     []int64{0, 2},
				Rooms: []Room{
					{
						RoomID: roomIDs[0],
					},
					{
						RoomID: roomIDs[1],
					},
					{
						RoomID: roomIDs[2],
					},
				},
			},
		},
	})
	// add on a different non-overlapping range
	res, err = cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 2}, {4, 6},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, true, res, &Response{
		Count: int64(len(rooms)),
		Ops: []ResponseOp{
			&ResponseOpRange{
				Operation: "SYNC",
				Range:     []int64{4, 6},
				Rooms: []Room{
					{
						RoomID: roomIDs[4],
					},
					{
						RoomID: roomIDs[5],
					},
					{
						RoomID: roomIDs[6],
					},
				},
			},
		},
	})

	// pull room 8 to position 0 should result in DELETE[6] and INSERT[0]
	// 0,1,2,3,4,5,6,7,8,9
	// `----`  `----`
	// `    `  `    `
	// 8,0,1,2,3,4,5,6,7,9
	//
	globalCache.OnNewEvents(roomIDs[8], []json.RawMessage{
		testutils.NewEvent(t, "unimportant", "me", struct{}{}, timestampNow+2000),
	}, 1)

	res, err = cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 2}, {4, 6},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, true, res, &Response{
		Count: int64(len(rooms)),
		Ops: []ResponseOp{
			&ResponseOpSingle{
				Operation: "DELETE",
				Index:     intPtr(6),
			},
			&ResponseOpSingle{
				Operation: "INSERT",
				Index:     intPtr(0),
				Room: &Room{
					RoomID: roomIDs[8],
				},
			},
		},
	})

	// pull room 9 to position 3 should result in DELETE[6] and INSERT[4] with room 2
	// 0,1,2,3,4,5,6,7,8,9 index
	// 8,0,1,2,3,4,5,6,7,9 room
	// `----`  `----`
	// `    `  `    `
	// 8,0,1,9,2,3,4,5,6,7 room
	middleTimestamp := int64((roomIDToRoom[roomIDs[1]].LastMessageTimestamp + roomIDToRoom[roomIDs[2]].LastMessageTimestamp) / 2)
	globalCache.OnNewEvents(roomIDs[9], []json.RawMessage{
		testutils.NewEvent(t, "unimportant", "me", struct{}{}, middleTimestamp),
	}, 1)
	res, err = cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 2}, {4, 6},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, true, res, &Response{
		Count: int64(len(rooms)),
		Ops: []ResponseOp{
			&ResponseOpSingle{
				Operation: "DELETE",
				Index:     intPtr(6),
			},
			&ResponseOpSingle{
				Operation: "INSERT",
				Index:     intPtr(4),
				Room: &Room{
					RoomID: roomIDs[2],
				},
			},
		},
	})
}

// Regression test for https://github.com/matrix-org/sync-v3/commit/732ea46f1ccde2b6a382e0f849bbd166b80900ed
func TestBumpToOutsideRange(t *testing.T) {
	connID := ConnID{
		SessionID: "s",
		DeviceID:  "d",
	}
	userID := "@TestBumpToOutsideRange_alice:localhost"
	timestampNow := int64(1632131678061)
	roomA := newSortableRoom("!a:localhost", timestampNow)
	roomB := newSortableRoom("!b:localhost", timestampNow-1000)
	roomC := newSortableRoom("!c:localhost", timestampNow-2000)
	roomD := newSortableRoom("!d:localhost", timestampNow-3000)
	globalCache := NewGlobalCache(nil)
	globalCache.AssignRoom(roomA)
	globalCache.AssignRoom(roomB)
	globalCache.AssignRoom(roomC)
	globalCache.AssignRoom(roomD)
	globalCache.jrt.UserJoinedRoom(userID, roomA.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomB.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomC.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomD.RoomID)
	globalCache.LoadJoinedRoomsOverride = func(userID string) (pos int64, joinedRooms []SortableRoom, err error) {
		return 1, []SortableRoom{
			roomA, roomB, roomC, roomD,
		}, nil
	}
	userCache := NewUserCache(userID, nil)
	userCache.LazyRoomDataOverride = mockLazyRoomOverride
	cs := NewConnState(userID, userCache, globalCache)
	// Ask for A,B
	res, err := cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 1},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, true, res, &Response{
		Count: int64(4),
		Ops: []ResponseOp{
			&ResponseOpRange{
				Operation: "SYNC",
				Range:     []int64{0, 1},
				Rooms: []Room{
					{
						RoomID: roomA.RoomID,
					},
					{
						RoomID: roomB.RoomID,
					},
				},
			},
		},
	})

	// D gets bumped to C's position but it's still outside the range so nothing should happen
	globalCache.OnNewEvents(roomD.RoomID, []json.RawMessage{
		testutils.NewEvent(t, "unimportant", "me", struct{}{}, roomC.LastMessageTimestamp+2),
	}, 1)

	// expire the context after 10ms so we don't wait forevar
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	res, err = cs.HandleIncomingRequest(ctx, connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 1},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	if len(res.Ops) > 0 {
		t.Errorf("response returned ops, expected none")
	}
}

// Test that room subscriptions can be made and that events are pushed for them.
func TestConnStateRoomSubscriptions(t *testing.T) {
	connID := ConnID{
		SessionID: "s",
		DeviceID:  "d",
	}
	userID := "@TestConnStateRoomSubscriptions_alice:localhost"
	timestampNow := int64(1632131678061)
	roomA := newSortableRoom("!a:localhost", timestampNow)
	roomB := newSortableRoom("!b:localhost", timestampNow-1000)
	roomC := newSortableRoom("!c:localhost", timestampNow-2000)
	roomD := newSortableRoom("!d:localhost", timestampNow-3000)
	roomIDs := []string{roomA.RoomID, roomB.RoomID, roomC.RoomID, roomD.RoomID}
	globalCache := NewGlobalCache(nil)
	globalCache.AssignRoom(roomA)
	globalCache.AssignRoom(roomB)
	globalCache.AssignRoom(roomC)
	globalCache.AssignRoom(roomD)
	globalCache.jrt.UserJoinedRoom(userID, roomA.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomB.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomC.RoomID)
	globalCache.jrt.UserJoinedRoom(userID, roomD.RoomID)
	globalCache.LoadJoinedRoomsOverride = func(userID string) (pos int64, joinedRooms []SortableRoom, err error) {
		return 1, []SortableRoom{
			roomA, roomB, roomC, roomD,
		}, nil
	}
	userCache := NewUserCache(userID, nil)
	userCache.LazyRoomDataOverride = func(loadPos int64, roomIDs []string, maxTimelineEvents int) map[string]UserRoomData {
		result := make(map[string]UserRoomData)
		for _, roomID := range roomIDs {
			result[roomID] = UserRoomData{
				Timeline: []json.RawMessage{
					globalCache.LoadRoom(roomID).LastEventJSON,
				},
			}
		}
		return result
	}
	cs := NewConnState(userID, userCache, globalCache)
	// subscribe to room D
	res, err := cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		RoomSubscriptions: map[string]RoomSubscription{
			roomD.RoomID: {
				TimelineLimit: 20,
			},
		},
		Rooms: SliceRanges([][2]int64{
			{0, 1},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, false, res, &Response{
		Count: int64(len(roomIDs)),
		RoomSubscriptions: map[string]Room{
			roomD.RoomID: {
				RoomID: roomD.RoomID,
				Name:   roomD.Name,
				Timeline: []json.RawMessage{
					roomD.LastEventJSON,
				},
			},
		},
		Ops: []ResponseOp{
			&ResponseOpRange{
				Operation: "SYNC",
				Range:     []int64{0, 1},
				Rooms: []Room{
					{
						RoomID: roomA.RoomID,
						Name:   roomA.Name,
						Timeline: []json.RawMessage{
							roomA.LastEventJSON,
						},
					},
					{
						RoomID: roomB.RoomID,
						Name:   roomB.Name,
						Timeline: []json.RawMessage{
							roomB.LastEventJSON,
						},
					},
				},
			},
		},
	})
	// room D gets a new event
	newEvent := testutils.NewEvent(t, "unimportant", "me", struct{}{}, timestampNow+2000)
	globalCache.OnNewEvents(roomD.RoomID, []json.RawMessage{
		newEvent,
	}, 1)
	// we should get this message even though it's not in the range because we are subscribed to this room.
	res, err = cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		Rooms: SliceRanges([][2]int64{
			{0, 1},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, false, res, &Response{
		Count: int64(len(roomIDs)),
		RoomSubscriptions: map[string]Room{
			roomD.RoomID: {
				RoomID: roomD.RoomID,
				Timeline: []json.RawMessage{
					newEvent,
				},
			},
		},
		// TODO: index markers as this new event should bump D into the tracked range
	})

	// now swap to room C
	res, err = cs.HandleIncomingRequest(context.Background(), connID, &Request{
		Sort: []string{SortByRecency},
		RoomSubscriptions: map[string]RoomSubscription{
			roomC.RoomID: {
				TimelineLimit: 20,
			},
		},
		UnsubscribeRooms: []string{roomD.RoomID},
		Rooms: SliceRanges([][2]int64{
			{0, 1},
		}),
	})
	if err != nil {
		t.Fatalf("HandleIncomingRequest returned error : %s", err)
	}
	checkResponse(t, false, res, &Response{
		Count: int64(len(roomIDs)),
		RoomSubscriptions: map[string]Room{
			roomC.RoomID: {
				RoomID: roomC.RoomID,
				Name:   roomC.Name,
				Timeline: []json.RawMessage{
					roomC.LastEventJSON,
				},
			},
		},
	})
}

func checkResponse(t *testing.T, checkRoomIDsOnly bool, got, want *Response) {
	t.Helper()
	if want.Count > 0 {
		if got.Count != want.Count {
			t.Errorf("response Count: got %d want %d", got.Count, want.Count)
		}
	}
	if len(want.Ops) > 0 {
		t.Logf("got %v", serialise(t, got))
		t.Logf("want %v", serialise(t, want))
		defer func() {
			t.Helper()
			if !t.Failed() {
				t.Logf("OK!")
			}
		}()
		if len(got.Ops) != len(want.Ops) {
			t.Fatalf("got %d ops, want %d", len(got.Ops), len(want.Ops))
		}
		for i, wantOpVal := range want.Ops {
			gotOp := got.Ops[i]
			if gotOp.Op() != wantOpVal.Op() {
				t.Errorf("operation i=%d got '%s' want '%s'", i, gotOp.Op(), wantOpVal.Op())
			}
			switch wantOp := wantOpVal.(type) {
			case *ResponseOpRange:
				gotOpRange, ok := gotOp.(*ResponseOpRange)
				if !ok {
					t.Fatalf("operation i=%d (%s) want type ResponseOpRange but it isn't", i, gotOp.Op())
				}
				if !reflect.DeepEqual(gotOpRange.Range, wantOp.Range) {
					t.Errorf("operation i=%d (%s) got range %v want range %v", i, gotOp.Op(), gotOpRange.Range, wantOp.Range)
				}
				if len(gotOpRange.Rooms) != len(wantOp.Rooms) {
					t.Fatalf("operation i=%d (%s) got %d rooms in array, want %d", i, gotOp.Op(), len(gotOpRange.Rooms), len(wantOp.Rooms))
				}
				for j := range wantOp.Rooms {
					checkRoomsEqual(t, checkRoomIDsOnly, &gotOpRange.Rooms[j], &wantOp.Rooms[j])
				}
			case *ResponseOpSingle:
				gotOpSingle, ok := gotOp.(*ResponseOpSingle)
				if !ok {
					t.Fatalf("operation i=%d (%s) want type ResponseOpSingle but it isn't", i, gotOp.Op())
				}
				if *gotOpSingle.Index != *wantOp.Index {
					t.Errorf("operation i=%d (%s) single op on index %d want index %d", i, gotOp.Op(), *gotOpSingle.Index, *wantOp.Index)
				}
				checkRoomsEqual(t, checkRoomIDsOnly, gotOpSingle.Room, wantOp.Room)
			}
		}
	}
	if len(want.RoomSubscriptions) > 0 {
		if len(want.RoomSubscriptions) != len(got.RoomSubscriptions) {
			t.Errorf("wrong number of room subs returned, got %d want %d", len(got.RoomSubscriptions), len(want.RoomSubscriptions))
		}
		for roomID, wantData := range want.RoomSubscriptions {
			gotData, ok := got.RoomSubscriptions[roomID]
			if !ok {
				t.Errorf("wanted room subscription for %s but it was not returned", roomID)
				continue
			}
			checkRoomsEqual(t, checkRoomIDsOnly, &gotData, &wantData)
		}
	}
}

func checkRoomsEqual(t *testing.T, checkRoomIDsOnly bool, got, want *Room) {
	t.Helper()
	if got == nil && want == nil {
		return // e.g DELETE ops
	}
	if (got == nil && want != nil) || (want == nil && got != nil) {
		t.Fatalf("nil room, got %+v want %+v", got, want)
	}
	if checkRoomIDsOnly {
		if got.RoomID != want.RoomID {
			t.Fatalf("got room '%s' want room '%s'", got.RoomID, want.RoomID)
		}
		return
	}
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("cannot marshal got room: %s", err)
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("cannot marshal want room: %s", err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Errorf("rooms do not match,\ngot  %s want %s", string(gotBytes), string(wantBytes))
	}
}

func serialise(t *testing.T, thing interface{}) string {
	b, err := json.Marshal(thing)
	if err != nil {
		t.Fatalf("cannot serialise: %s", err)
	}
	return string(b)
}

func intPtr(val int) *int {
	return &val
}
