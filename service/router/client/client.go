package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	goclient "github.com/micro/go-micro/v3/client"
	"github.com/micro/go-micro/v3/router"
	"github.com/micro/micro/v3/service/client"
	"github.com/micro/micro/v3/service/errors"
	pb "github.com/micro/micro/v3/service/router/proto"
)

var (
	// name of the router service
	name = "router"
)

type svc struct {
	sync.RWMutex
	opts       router.Options
	callOpts   []goclient.CallOption
	router     pb.RouterService
	table      *table
	exit       chan bool
	errChan    chan error
	advertChan chan *router.Advert
}

// NewRouter creates new service router and returns it
func NewRouter(opts ...router.Option) router.Router {
	// get default options
	options := router.DefaultOptions()

	// apply requested options
	for _, o := range opts {
		o(&options)
	}

	s := &svc{
		opts:   options,
		router: pb.NewRouterService(name, client.DefaultClient),
	}

	// set the router address to call
	if len(options.Address) > 0 {
		s.callOpts = []goclient.CallOption{
			goclient.WithAddress(options.Address),
			goclient.WithAuthToken(),
		}
	}
	// set the table
	s.table = &table{
		pb.NewTableService(name, client.DefaultClient),
		s.callOpts,
	}

	return s
}

// Init initializes router with given options
func (s *svc) Init(opts ...router.Option) error {
	s.Lock()
	defer s.Unlock()

	for _, o := range opts {
		o(&s.opts)
	}

	return nil
}

// Options returns router options
func (s *svc) Options() router.Options {
	s.Lock()
	opts := s.opts
	s.Unlock()

	return opts
}

// Table returns routing table
func (s *svc) Table() router.Table {
	return s.table
}

func (s *svc) advertiseEvents(advertChan chan *router.Advert, stream pb.Router_AdvertiseService) error {
	go func() {
		<-s.exit
		stream.Close()
	}()

	var advErr error

	for {
		resp, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				advErr = err
			}
			break
		}

		events := make([]*router.Event, len(resp.Events))
		for i, event := range resp.Events {
			route := router.Route{
				Service:  event.Route.Service,
				Address:  event.Route.Address,
				Gateway:  event.Route.Gateway,
				Network:  event.Route.Network,
				Link:     event.Route.Link,
				Metric:   event.Route.Metric,
				Metadata: event.Route.Metadata,
			}

			events[i] = &router.Event{
				Id:        event.Id,
				Type:      router.EventType(event.Type),
				Timestamp: time.Unix(0, event.Timestamp),
				Route:     route,
			}
		}

		advert := &router.Advert{
			Id:        resp.Id,
			Type:      router.AdvertType(resp.Type),
			Timestamp: time.Unix(0, resp.Timestamp),
			TTL:       time.Duration(resp.Ttl),
			Events:    events,
		}

		select {
		case advertChan <- advert:
		case <-s.exit:
			close(advertChan)
			return nil
		}
	}

	// close the channel on exit
	close(advertChan)

	return advErr
}

// Advertise advertises routes to the network
func (s *svc) Advertise() (<-chan *router.Advert, error) {
	s.Lock()
	defer s.Unlock()

	stream, err := s.router.Advertise(context.Background(), &pb.Request{}, s.callOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed getting advert stream: %s", err)
	}

	// create advertise and event channels
	advertChan := make(chan *router.Advert)
	go s.advertiseEvents(advertChan, stream)

	return advertChan, nil
}

// Process processes incoming adverts
func (s *svc) Process(advert *router.Advert) error {
	events := make([]*pb.Event, 0, len(advert.Events))
	for _, event := range advert.Events {
		route := &pb.Route{
			Service:  event.Route.Service,
			Address:  event.Route.Address,
			Gateway:  event.Route.Gateway,
			Network:  event.Route.Network,
			Link:     event.Route.Link,
			Metric:   event.Route.Metric,
			Metadata: event.Route.Metadata,
		}
		e := &pb.Event{
			Id:        event.Id,
			Type:      pb.EventType(event.Type),
			Timestamp: event.Timestamp.UnixNano(),
			Route:     route,
		}
		events = append(events, e)
	}

	advertReq := &pb.Advert{
		Id:        s.Options().Id,
		Type:      pb.AdvertType(advert.Type),
		Timestamp: advert.Timestamp.UnixNano(),
		Events:    events,
	}

	if _, err := s.router.Process(context.Background(), advertReq, s.callOpts...); err != nil {
		return err
	}

	return nil
}

// Remote router cannot be closed
func (s *svc) Close() error {
	s.Lock()
	defer s.Unlock()

	select {
	case <-s.exit:
		return nil
	default:
		close(s.exit)
	}

	return nil
}

// Lookup looks up routes in the routing table and returns them
func (s *svc) Lookup(q ...router.QueryOption) ([]router.Route, error) {
	// call the router
	query := router.NewQuery(q...)

	resp, err := s.router.Lookup(context.Background(), &pb.LookupRequest{
		Query: &pb.Query{
			Service: query.Service,
			Gateway: query.Gateway,
			Network: query.Network,
		},
	}, s.callOpts...)

	if verr := errors.Parse(err); verr != nil && verr.Code == http.StatusNotFound {
		return nil, router.ErrRouteNotFound
	} else if err != nil {
		return nil, err
	}

	routes := make([]router.Route, len(resp.Routes))
	for i, route := range resp.Routes {
		routes[i] = router.Route{
			Service:  route.Service,
			Address:  route.Address,
			Gateway:  route.Gateway,
			Network:  route.Network,
			Link:     route.Link,
			Metric:   route.Metric,
			Metadata: route.Metadata,
		}
	}

	return routes, nil
}

// Watch returns a watcher which allows to track updates to the routing table
func (s *svc) Watch(opts ...router.WatchOption) (router.Watcher, error) {
	rsp, err := s.router.Watch(context.Background(), &pb.WatchRequest{}, s.callOpts...)
	if err != nil {
		return nil, err
	}
	options := router.WatchOptions{
		Service: "*",
	}
	for _, o := range opts {
		o(&options)
	}
	return newWatcher(rsp, options)
}

// Returns the router implementation
func (s *svc) String() string {
	return "service"
}