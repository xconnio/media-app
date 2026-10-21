package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/appkit"
	mediaapp "github.com/xconnio/media-app"
	"github.com/xconnio/xconn-go"
)

const (
	procedureKeyExchange = "io.xconn.media.key.exchange"
	procedureIndexQuery  = "io.xconn.media.index.query"
)

type app struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	KeyExchange string   `json:"key_exchange"`
	Requires    []string `json:"requires"`
	DefaultOff  bool     `json:"default_off"`
	categories  []string
}

//nolint:gochecknoglobals // fixed list
var apps = []app{
	{ID: "pictures", Name: "Pictures", KeyExchange: procedureKeyExchange, Requires: []string{"files"},
		DefaultOff: true, categories: []string{mediaapp.CategoryImages}},
	{ID: "videos", Name: "Videos", KeyExchange: procedureKeyExchange, Requires: []string{"files"},
		DefaultOff: true, categories: []string{mediaapp.CategoryVideos}},
	{ID: "documents", Name: "Documents", KeyExchange: procedureKeyExchange, Requires: []string{"files"},
		DefaultOff: true, categories: []string{mediaapp.CategoryPDFs, mediaapp.CategoryTexts, mediaapp.CategoryDocuments}},
}

const reconnectDelay = 2 * time.Second

func main() {
	cfgDirectory, err := appkit.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := &media{cfgDirectory: cfgDirectory, enc: appkit.NewEncryption()}
	defer m.setIndexing(ctx, false)

	uri := appkit.SocketURI(cfgDirectory)
	for ctx.Err() == nil {
		if err := m.serve(ctx, uri); err != nil {
			log.Printf("media-app: %v, retrying in %v", err, reconnectDelay)
		}
		select {
		case <-ctx.Done():
		case <-time.After(reconnectDelay):
		}
	}
}

type media struct {
	cfgDirectory string
	enc          *appkit.Encryption

	mu         sync.Mutex
	session    *xconn.Session
	query      *xconn.RegisterResponse
	index      *mediaapp.IndexService
	cancel     context.CancelFunc
	categories []string
}

func (m *media) serve(ctx context.Context, uri string) error {
	session, err := xconn.ConnectAnonymous(ctx, uri, appkit.LocalRealm)
	if err != nil {
		return fmt.Errorf("could not reach deskconnd: %w", err)
	}
	defer func() { _ = session.Leave() }()

	m.mu.Lock()
	m.session, m.query = session, nil
	m.mu.Unlock()
	if err := m.enc.Register(session, procedureKeyExchange); err != nil {
		return err
	}
	subResp := session.Subscribe(appkit.TopicCapabilitiesChanged, func(event *xconn.Event) {
		if len(event.Args()) > 0 {
			m.applyCapabilities(ctx, event.Args()[0])
		}
	}).Do()
	if subResp.Err != nil {
		return subResp.Err
	}
	payload, err := json.Marshal(apps)
	if err != nil {
		return err
	}
	if resp := session.Call(appkit.ProcedureAppsRegister).Args(payload).Do(); resp.Err != nil {
		return fmt.Errorf("could not register apps: %w", resp.Err)
	}
	listResp := session.Call(appkit.ProcedureCapabilitiesList).Do()
	if listResp.Err != nil {
		return listResp.Err
	}
	if len(listResp.Args()) > 0 {
		m.applyCapabilities(ctx, listResp.Args()[0])
	}
	log.Println("media-app: connected to deskconnd")

	select {
	case <-ctx.Done():
		return nil
	case <-session.Done():
		return errors.New("disconnected from deskconnd")
	}
}

func (m *media) applyCapabilities(ctx context.Context, list any) {
	data, err := json.Marshal(list)
	if err != nil {
		return
	}
	var states []struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(data, &states); err != nil {
		log.Printf("media-app: unexpected apps list: %v", err)
		return
	}
	var categories []string
	for _, state := range states {
		i := slices.IndexFunc(apps, func(a app) bool { return a.ID == state.ID })
		if state.Enabled && i >= 0 {
			categories = append(categories, apps[i].categories...)
		}
	}
	m.mu.Lock()
	m.categories = categories
	m.mu.Unlock()
	m.setIndexing(ctx, len(categories) > 0)
	if err := m.setServing(len(categories) > 0); err != nil {
		log.Printf("media-app: %v", err)
	}
}

func (m *media) setServing(on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on == (m.query != nil) {
		return nil
	}
	if !on {
		err := m.query.Unregister()
		m.query = nil
		return err
	}
	resp := m.session.Register(procedureIndexQuery, m.enc.Encrypted(m.handleQuery)).Do()
	if resp.Err != nil {
		return resp.Err
	}
	m.query = &resp
	return nil
}

func (m *media) setIndexing(ctx context.Context, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on == (m.index != nil) {
		return
	}
	if !on {
		m.cancel()
		m.index.Close()
		m.index, m.cancel = nil, nil
		log.Println("media-app: indexing stopped, no media app is enabled")
		return
	}

	index, err := mediaapp.NewIndexService(m.cfgDirectory)
	if err != nil {
		log.Printf("media-app: failed to start indexing: %v", err)
		return
	}
	indexCtx, cancel := context.WithCancel(ctx)
	index.Start(indexCtx)
	m.index, m.cancel = index, cancel
	log.Println("media-app: indexing started")
}

func (m *media) handleQuery(_ context.Context, req []byte) (any, error) {
	var args struct {
		Categories []string          `json:"categories"`
		Cursor     map[string]string `json:"cursor"`
		Limit      int               `json:"limit"`
	}
	if err := json.Unmarshal(req, &args); err != nil {
		return nil, &xconn.Error{URI: appkit.ErrInvalidArgument, Args: []any{err.Error()}}
	}

	m.mu.Lock()
	index, enabled := m.index, m.categories
	m.mu.Unlock()
	if index == nil {
		return nil, errors.New("no media app is enabled")
	}
	for _, category := range args.Categories {
		if !slices.Contains(enabled, category) {
			return nil, &xconn.Error{URI: appkit.ErrNotAuthorized, Args: []any{"category " + category + " is disabled"}}
		}
	}
	if len(args.Categories) == 0 {
		args.Categories = enabled
	}
	return index.Query(args.Categories, args.Cursor, args.Limit)
}
