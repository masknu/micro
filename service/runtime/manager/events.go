package manager

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	gorun "github.com/micro/go-micro/v3/runtime"
	"github.com/micro/go-micro/v3/runtime/local/source/git"
	"github.com/micro/go-micro/v3/util/tar"
	"github.com/micro/micro/v3/internal/namespace"
	"github.com/micro/micro/v3/service/client"
	"github.com/micro/micro/v3/service/logger"
	"github.com/micro/micro/v3/service/runtime"
	"github.com/micro/micro/v3/service/store"
)

var (
	// eventTTL is the duration events will perist in the store before expiring
	eventTTL = time.Minute * 10
	// eventPollFrequency is the max frequency the manager will check for new events in the store
	eventPollFrequency = time.Minute
)

const (
	// eventPrefix is prefixed to the key for event records
	eventPrefix = "event/"
	// eventProcessedPrefix is prefixed to the key for tracking event processing
	eventProcessedPrefix = "processed/"
)

// publishEvent will write the event to the global store and immediately process the event
func (m *manager) publishEvent(eType gorun.EventType, srv *gorun.Service, opts *gorun.CreateOptions) error {
	e := &gorun.Event{
		ID:      uuid.New().String(),
		Type:    eType,
		Service: srv,
		Options: opts,
	}

	bytes, err := json.Marshal(e)
	if err != nil {
		return err
	}

	record := &store.Record{
		Key:    eventPrefix + e.ID,
		Value:  bytes,
		Expiry: eventTTL,
	}

	if err := store.Write(record); err != nil {
		return err
	}

	go m.processEvent(record.Key)
	return nil
}

// watchEvents polls the store for events periodically and processes them if they have not already
// done so
func (m *manager) watchEvents() {
	ticker := time.NewTicker(eventPollFrequency)

	for {
		// get the keys of the events
		events, err := store.Read("", store.Prefix(eventPrefix))
		if err != nil {
			logger.Warn("Error listing events: %v", err)
			continue
		}

		// loop through every event
		for _, ev := range events {
			logger.Debugf("Process Event: %v", ev.Key)
			m.processEvent(ev.Key)
		}

		<-ticker.C
	}
}

// processEvent will take an event key, verify it hasn't been consumed and then execute it. We pass
// the key and not the ID since the global store and the memory store use the same key prefix so there
// is not point stripping and then re-prefixing.
func (m *manager) processEvent(key string) {
	// check to see if the event has been processed before
	if _, err := m.fileCache.Read(eventProcessedPrefix + key); err != store.ErrNotFound {
		return
	}

	// lookup the event
	recs, err := store.Read(key)
	if err != nil {
		logger.Warnf("Error finding event %v: %v", key, err)
		return
	}
	var ev *gorun.Event
	if err := json.Unmarshal(recs[0].Value, &ev); err != nil {
		logger.Warnf("Error unmarshaling event %v: %v", key, err)
	}

	// determine the namespace
	if ev.Options == nil {
		ev.Options = &runtime.CreateOptions{}
	}
	if len(ev.Options.Namespace) == 0 {
		ev.Options.Namespace = namespace.DefaultNamespace
	}

	// log the event
	logger.Infof("Processing %v event for service %v:%v in namespace %v", ev.Type, ev.Service.Name, ev.Service.Version, ev.Options.Namespace)

	// apply the event to the managed runtime
	switch ev.Type {
	case gorun.Delete:
		err = runtime.Delete(ev.Service, gorun.DeleteNamespace(ev.Options.Namespace))
	case gorun.Update:
		err = runtime.Update(ev.Service, gorun.UpdateNamespace(ev.Options.Namespace))
	case gorun.Create:
		// generate an auth account for the service to use
		err = m.handleCreateEvent(ev.Service, ev.Options)
	}

	// if there was an error update the status in the cache
	if err != nil {
		logger.Warnf("Error processing %v event for service %v:%v (src: %v) in namespace %v: %v",
			ev.Type,
			ev.Service.Name,
			ev.Service.Version,
			ev.Service.Source,
			ev.Options.Namespace,
			err,
		)

		ev.Service.Status = gorun.Error
		ev.Service.Metadata = map[string]string{"error": err.Error()}
		m.cacheStatus(ev.Options.Namespace, ev.Service)
	} else if ev.Type != gorun.Delete {
		m.cacheStatus(ev.Options.Namespace, ev.Service)
	}

	// write to the store indicating the event has been consumed. We double the ttl to safely know the
	// event will expire before this record
	m.fileCache.Write(&store.Record{Key: eventProcessedPrefix + key, Expiry: eventTTL * 2})

}

// runtimeEnv returns the environment variables which should  be used when creating a service.
func (m *manager) runtimeEnv(srv *gorun.Service, options *gorun.CreateOptions) []string {
	setEnv := func(p []string, env map[string]string) {
		for _, v := range p {
			parts := strings.Split(v, "=")
			if len(parts) <= 1 {
				continue
			}
			env[parts[0]] = strings.Join(parts[1:], "=")
		}
	}

	// overwrite any values
	env := map[string]string{
		// ensure a profile for the services isn't set, they
		// should use the default RPC clients
		"MICRO_PROFILE": "",
		// pass the service's name and version
		"MICRO_SERVICE_NAME":    nameFromService(srv.Name),
		"MICRO_SERVICE_VERSION": srv.Version,
		// set the proxy for the service to use (e.g. micro network)
		// using the proxy which has been configured for the runtime
		"MICRO_PROXY": client.DefaultClient.Options().Proxy,
	}

	// bind to port 8080, this is what the k8s tcp readiness check will use
	if runtime.DefaultRuntime.String() == "kubernetes" {
		env["MICRO_SERVICE_ADDRESS"] = ":8080"
	}

	// set the env vars provided
	setEnv(options.Env, env)

	// set the service namespace
	if len(options.Namespace) > 0 {
		env["MICRO_NAMESPACE"] = options.Namespace
	}

	// create a new env
	var vars []string
	for k, v := range env {
		vars = append(vars, k+"="+v)
	}

	// setup the runtime env
	return vars
}

// if a service is run from directory "/test/foo", the service
// will register as foo
func nameFromService(name string) string {
	comps := strings.Split(name, "/")
	if len(comps) == 0 {
		return ""
	}
	return comps[len(comps)-1]
}

func (m *manager) handleCreateEvent(srv *runtime.Service, opts *runtime.CreateOptions) error {
	if strings.HasPrefix(srv.Source, "source://") {
		// source is in the blob store. create a tmp dir to store it in
		dir, err := ioutil.TempDir(os.TempDir(), fmt.Sprintf("source-%v-*", srv.Name))
		if err != nil {
			return err
		}

		// read the source from the blob store
		blob, err := store.DefaultBlobStore.Read(srv.Source)
		if err != nil {
			return err
		}

		// unarchive the source into the destination
		if err := tar.Unarchive(blob, dir); err != nil {
			return err
		}

		srv.Source = dir
	} else {
		// source is a git remote, parse the source to split the repo and folder
		src, err := git.ParseSource(srv.Source)
		if err != nil {
			return err
		}

		// checkout the source
		tmpdir, err := ioutil.TempDir(os.TempDir(), "source-*")
		if err != nil {
			return err
		}
		if err := git.CheckoutSource(tmpdir, src, opts.Secrets); err != nil {
			return err
		}

		// the git package rewrites the git source location, todo: refactor this to write directly to
		// the tmpdirs
		srv.Source = src.FullPath
		opts.Entrypoint = src.Folder
	}

	// generate an auth account for the service to use
	acc, err := m.generateAccount(srv, opts.Namespace)
	if err != nil {
		return err
	}

	// construct the options
	options := []gorun.CreateOption{
		gorun.CreateImage(opts.Image),
		gorun.CreateType(opts.Type),
		gorun.CreateNamespace(opts.Namespace),
		gorun.CreateEntrypoint(opts.Entrypoint),
		gorun.WithArgs(opts.Args...),
		gorun.WithCommand(opts.Command...),
		gorun.WithEnv(m.runtimeEnv(srv, opts)),
	}

	// inject the credentials into the service if present
	if len(acc.ID) > 0 && len(acc.Secret) > 0 {
		options = append(options, gorun.WithSecret("MICRO_AUTH_ID", acc.ID))
		options = append(options, gorun.WithSecret("MICRO_AUTH_SECRET", acc.Secret))
	}

	// add the secrets
	for key, value := range opts.Secrets {
		options = append(options, gorun.WithSecret(key, value))
	}

	// create the service
	return runtime.Create(srv, options...)
}
