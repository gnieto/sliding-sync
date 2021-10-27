package sync3

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/sync-v3/internal"
	"github.com/matrix-org/sync-v3/state"
	"github.com/tidwall/gjson"
)

type GlobalCache struct {
	LoadJoinedRoomsOverride func(userID string) (pos int64, joinedRooms []SortableRoom, err error)

	// inserts are done by v2 poll loops, selects are done by v3 request threads
	// there are lots of overlapping keys as many users (threads) can be joined to the same room (key)
	// hence you must lock this with `mu` before r/w
	globalRoomInfo   map[string]*SortableRoom
	globalRoomInfoMu *sync.RWMutex

	// TODO: keep this updated with live events
	roomIDToHeroInfo map[string]internal.HeroInfo

	// for loading room state not held in-memory
	store *state.Storage

	id int
}

func NewGlobalCache(store *state.Storage) *GlobalCache {
	return &GlobalCache{
		globalRoomInfo:   make(map[string]*SortableRoom),
		globalRoomInfoMu: &sync.RWMutex{},
		store:            store,
		roomIDToHeroInfo: make(map[string]internal.HeroInfo),
	}
}

func (c *GlobalCache) LoadRoom(roomID string) *SortableRoom {
	c.globalRoomInfoMu.RLock()
	defer c.globalRoomInfoMu.RUnlock()
	sr := c.globalRoomInfo[roomID]
	if sr == nil {
		return nil
	}
	srCopy := *sr
	return &srCopy
}

func (c *GlobalCache) AssignRoom(r SortableRoom) {
	c.globalRoomInfoMu.Lock()
	defer c.globalRoomInfoMu.Unlock()
	c.globalRoomInfo[r.RoomID] = &r
}

func (c *GlobalCache) LoadJoinedRooms(userID string) (pos int64, joinedRooms []SortableRoom, err error) {
	if c.LoadJoinedRoomsOverride != nil {
		return c.LoadJoinedRoomsOverride(userID)
	}
	initialLoadPosition, err := c.store.LatestEventNID()
	if err != nil {
		return 0, nil, err
	}
	joinedRoomIDs, err := c.store.JoinedRoomsAfterPosition(userID, initialLoadPosition)
	if err != nil {
		return 0, nil, err
	}
	rooms := make([]SortableRoom, len(joinedRoomIDs))
	for i, roomID := range joinedRoomIDs {
		rooms[i] = *c.LoadRoom(roomID)
	}
	return initialLoadPosition, rooms, nil
}

func (c *GlobalCache) LoadRoomState(roomID string, loadPosition int64, requiredState [][2]string) []json.RawMessage {
	if len(requiredState) == 0 {
		return nil
	}
	if c.store == nil {
		return nil
	}
	// pull out unique event types and convert the required state into a map
	eventTypeSet := make(map[string]bool)
	requiredStateMap := make(map[string][]string) // event_type -> []state_key
	for _, rs := range requiredState {
		eventTypeSet[rs[0]] = true
		requiredStateMap[rs[0]] = append(requiredStateMap[rs[0]], rs[1])
	}
	eventTypes := make([]string, len(eventTypeSet))
	i := 0
	for et := range eventTypeSet {
		eventTypes[i] = et
		i++
	}
	stateEvents, err := c.store.RoomStateAfterEventPosition(roomID, loadPosition, eventTypes...)
	if err != nil {
		logger.Err(err).Str("room", roomID).Int64("pos", loadPosition).Msg("failed to load room state")
		return nil
	}
	var result []json.RawMessage
	for _, ev := range stateEvents {
		stateKeys := requiredStateMap[ev.Type]
		include := false
		for _, sk := range stateKeys {
			if sk == "*" { // wildcard
				include = true
				break
			}
			if sk == ev.StateKey {
				include = true
				break
			}
		}
		if include {
			result = append(result, ev.JSON)
		}
	}
	// TODO: cache?
	return result
}

// Startup will populate the cache by reading the database.
// Must be called prior to starting any v2 pollers else this operation can race. Consider:
//   - V2 poll loop started early
//   - Join event arrives, NID=50
//   - PopulateGlobalCache loads the latest NID=50, processes this join event in the process
//   - OnNewEvents is called with the join event
//   - join event is processed twice.
func (c *GlobalCache) Startup(store *state.Storage) error {
	latestEvents, err := store.SelectLatestEventInAllRooms()
	if err != nil {
		return fmt.Errorf("failed to load latest event for all rooms: %s", err)
	}
	// every room will be present here
	for _, ev := range latestEvents {
		room := &SortableRoom{
			RoomID: ev.RoomID,
		}
		room.LastMessageTimestamp = gjson.ParseBytes(ev.JSON).Get("origin_server_ts").Uint()
		c.AssignRoom(*room)
	}
	//roomIDToHeroInfo, err := store.HeroInfoForAllRooms()
	// load state events we care about for sync v3
	roomIDToStateEvents, err := store.CurrentStateEventsInAllRooms([]string{
		"m.room.name", "m.room.canonical_alias",
	})
	if err != nil {
		return fmt.Errorf("failed to load state events for all rooms: %s", err)
	}
	for roomID, stateEvents := range roomIDToStateEvents {
		room := c.LoadRoom(roomID)
		if room == nil {
			return fmt.Errorf("room %s has no latest event but does have state; this should be impossible", roomID)
		}
		for _, ev := range stateEvents {
			if ev.Type == "m.room.name" && ev.StateKey == "" {
				room.Name = gjson.ParseBytes(ev.JSON).Get("content.name").Str
			} else if ev.Type == "m.room.canonical_alias" && ev.StateKey == "" && room.Name == "" {
				room.Name = gjson.ParseBytes(ev.JSON).Get("content.alias").Str
			}
		}
		c.AssignRoom(*room)
		fmt.Printf("Room: %s - %s - %s \n", room.RoomID, room.Name, gomatrixserverlib.Timestamp(room.LastMessageTimestamp).Time())
	}

	return nil
}

// =================================================
// Listener function called dispatcher below
// =================================================

func (c *GlobalCache) OnNewEvent(
	ed *EventData,
) {
	// update global state
	c.globalRoomInfoMu.Lock()
	defer c.globalRoomInfoMu.Unlock()
	globalRoom := c.globalRoomInfo[ed.roomID]
	if globalRoom == nil {
		globalRoom = &SortableRoom{
			RoomID: ed.roomID,
		}
	}
	if ed.eventType == "m.room.name" && ed.stateKey != nil && *ed.stateKey == "" {
		globalRoom.Name = ed.content.Get("name").Str
	} else if ed.eventType == "m.room.canonical_alias" && ed.stateKey != nil && *ed.stateKey == "" && globalRoom.Name == "" {
		globalRoom.Name = ed.content.Get("alias").Str
	}
	globalRoom.LastMessageTimestamp = ed.timestamp
	c.globalRoomInfo[globalRoom.RoomID] = globalRoom
}
