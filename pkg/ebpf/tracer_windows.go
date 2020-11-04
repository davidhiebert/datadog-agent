// +build windows

package ebpf

import (
	"expvar"
	"fmt"
	"sync"
	"time"

	"github.com/DataDog/datadog-agent/pkg/network"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	defaultPollInterval = int(15)
)

var (
	expvarEndpoints map[string]*expvar.Map
	expvarTypes     = []string{"state", "driver_total_flow_stats", "driver_flow_handle_stats", "total_flows", "open_flows", "closed_flows", "more_data_errors"}
)

func init() {
	expvarEndpoints = make(map[string]*expvar.Map, len(expvarTypes))
	for _, name := range expvarTypes {
		expvarEndpoints[name] = expvar.NewMap(name)
	}
}

// Tracer struct for tracking network state and connections
type Tracer struct {
	config          *Config
	driverInterface *network.DriverInterface
	stopChan        chan struct{}
	state           network.State
	reverseDNS      network.ReverseDNS
	connLock        sync.Mutex

	timerInterval int

	// ticker for the polling interval for writing
	inTicker            *time.Ticker
	stopInTickerRoutine chan bool
}

// NewTracer returns an initialized tracer struct
func NewTracer(config *Config) (*Tracer, error) {
	di, err := network.NewDriverInterface(config.EnableMonotonicCount, config.DriverBufferSize)
	if err != nil {
		return nil, fmt.Errorf("could not create windows driver controller: %v", err)
	}

	state := network.NewState(
		config.ClientStateExpiry,
		config.MaxClosedConnectionsBuffered,
		config.MaxConnectionsStateBuffered,
		config.MaxDNSStatsBufferred,
	)

	tr := &Tracer{
		driverInterface: di,
		stopChan:        make(chan struct{}),
		timerInterval:   defaultPollInterval,
		state:           state,
		reverseDNS:      network.NewNullReverseDNS(),
	}

	go tr.expvarStats(tr.stopChan)
	return tr, nil
}

// Stop function stops running tracer
func (t *Tracer) Stop() {
	close(t.stopChan)
	err := t.driverInterface.Close()
	if err != nil {
		log.Errorf("error closing driver interface: %s", err)
	}
}

func (t *Tracer) expvarStats(exit <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// starts running the body immediately instead waiting for the first tick
	for range ticker.C {
		select {
		case <-exit:
			return
		default:
			stats, err := t.GetStats()
			if err != nil {
				continue
			}

			// Move state stats into proper field
			if states, ok := stats["state"]; ok {
				if telemetry, ok := states.(map[string]interface{})["telemetry"]; ok {
					stats["state"] = telemetry
				}
			}

			for name, stat := range stats {
				for metric, val := range stat.(map[string]int64) {
					currVal := &expvar.Int{}
					currVal.Set(val)
					expvarEndpoints[name].Set(snakeToCapInitialCamel(metric), currVal)
				}
			}
		}
	}
}

// printStats can be used to debug the stats we pull from the driver
func printStats(stats []network.ConnectionStats) {
	for _, stat := range stats {
		log.Infof("%v", stat)
	}
}

// GetActiveConnections returns all active connections
func (t *Tracer) GetActiveConnections(clientID string) (*network.Connections, error) {
	t.connLock.Lock()
	defer t.connLock.Unlock()

	activeConnStats, closedConnStats, err := t.driverInterface.GetConnectionStats()
	if err != nil {
		log.Errorf("failed to get connections")
		return nil, err
	}

	for _, connStat := range closedConnStats {
		t.state.StoreClosedConnection(&connStat)
	}

	// check for expired clients in the state
	t.state.RemoveExpiredClients(time.Now())
	conns := t.state.Connections(clientID, uint64(time.Now().Nanosecond()), activeConnStats, t.reverseDNS.GetDNSStats())
	return &network.Connections{Conns: conns}, nil
}

// GetStats returns a map of statistics about the current tracer's internal state
func (t *Tracer) GetStats() (map[string]interface{}, error) {
	driverStats, err := t.driverInterface.GetStats()
	if err != nil {
		log.Errorf("not printing driver stats: %v", err)
	}

	stateStats := t.state.GetStats()

	return map[string]interface{}{
		"state":                    stateStats,
		"total_flows":              driverStats["total_flows"],
		"open_flows":               driverStats["open_flows"],
		"closed_flows":             driverStats["closed_flows"],
		"more_data_errors":         driverStats["more_data_errors"],
		"driver_total_flow_stats":  driverStats["driver_total_flow_stats"],
		"driver_flow_handle_stats": driverStats["driver_flow_handle_stats"],
	}, nil
}

// DebugNetworkState returns a map with the current tracer's internal state, for debugging
func (t *Tracer) DebugNetworkState(_ string) (map[string]interface{}, error) {
	return nil, ErrNotImplemented
}

// DebugNetworkMaps returns all connections stored in the maps without modifications from network state
func (t *Tracer) DebugNetworkMaps() (*network.Connections, error) {
	return nil, ErrNotImplemented
}

// CurrentKernelVersion is not implemented on this OS for Tracer
func CurrentKernelVersion() (uint32, error) {
	return 0, ErrNotImplemented
}
