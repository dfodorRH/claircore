package indexer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"

	"github.com/quay/zlog"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/quay/claircore"
)

type LayerScanner struct {
	store Store

	// Maximum allowed in-flight scanners per Scan call
	inflight int64

	// Pre-constructed and configured scanners.
	ps  []PackageScanner
	ds  []DistributionScanner
	rs  []RepositoryScanner
	fis []FileScanner
}

// NewLayerScanner is the constructor for a LayerScanner.
//
// The provided Context is only used for the duration of the call.
func NewLayerScanner(ctx context.Context, concurrent int, opts *Options) (*LayerScanner, error) {
	ctx = zlog.ContextWithValues(ctx, "component", "indexer.NewLayerScanner")
	zlog.Info(ctx).Msg("NewLayerScanner: constructing a new layer-scanner")
	switch {
	case concurrent < 1:
		zlog.Warn(ctx).
			Int("value", concurrent).
			Msg("rectifying nonsense 'concurrent' argument")
		fallthrough
	case concurrent == 0:
		concurrent = runtime.GOMAXPROCS(0)
	}

	ps, ds, rs, fs, err := EcosystemsToScanners(ctx, opts.Ecosystems)
	if err != nil {
		return nil, fmt.Errorf("failed to extract scanners from ecosystems: %v", err)
	}

	return &LayerScanner{
		store:    opts.Store,
		inflight: int64(concurrent),
		ps:       configAndFilter(ctx, opts, ps),
		ds:       configAndFilter(ctx, opts, ds),
		rs:       configAndFilter(ctx, opts, rs),
		fis:      configAndFilter(ctx, opts, fs),
	}, nil
}

func configAndFilter[S VersionedScanner](ctx context.Context, opts *Options, ss []S) []S {
	i := 0
	for _, s := range ss {
		n := s.Name()
		var cfgMap map[string]func(interface{}) error
		switch k := s.Kind(); k {
		case "package":
			cfgMap = opts.ScannerConfig.Package
		case "repository":
			cfgMap = opts.ScannerConfig.Repo
		case "distribution":
			cfgMap = opts.ScannerConfig.Dist
		case "file":
			cfgMap = opts.ScannerConfig.File
		default:
			zlog.Warn(ctx).
				Str("kind", k).
				Str("scanner", n).
				Msg("unknown scanner kind")
			continue
		}

		f, haveCfg := cfgMap[n]
		if !haveCfg {
			f = func(interface{}) error { return nil }
		}
		cs, csOK := interface{}(s).(ConfigurableScanner)
		rs, rsOK := interface{}(s).(RPCScanner)
		switch {
		case haveCfg && !csOK && !rsOK:
			zlog.Warn(ctx).
				Str("scanner", n).
				Msg("configuration present for an unconfigurable scanner, skipping")
		case csOK && rsOK:
			fallthrough
		case !csOK && rsOK:
			if err := rs.Configure(ctx, f, opts.Client); err != nil {
				zlog.Error(ctx).
					Str("scanner", n).
					Err(err).
					Msg("configuration failed")
				continue
			}
		case csOK && !rsOK:
			if err := cs.Configure(ctx, f); err != nil {
				zlog.Error(ctx).
					Str("scanner", n).
					Err(err).
					Msg("configuration failed")
				continue
			}
		}
		ss[i] = s
		i++
	}
	ss = ss[:i]
	return ss
}

// Scan performs a concurrency controlled scan of each layer by each configured
// scanner, indexing the results on successful completion.
//
// Scan will launch all layer scan goroutines immediately and then only allow
// the configured limit to proceed.
//
// The provided Context controls cancellation for all scanners. The first error
// reported halts all work and is returned from Scan.
func (ls *LayerScanner) Scan(ctx context.Context, manifest claircore.Digest, layers []*claircore.Layer) error {
	ctx = zlog.ContextWithValues(ctx,
		"component", "indexer/LayerScanner.Scan",
		"manifest", manifest.String())

	sem := semaphore.NewWeighted(ls.inflight)
	g, ctx := errgroup.WithContext(ctx)
	// Launch is a closure to capture the loop variables and then call the
	// scanLayer method.
	launch := func(l *claircore.Layer, s VersionedScanner) func() error {
		return func() error {
			if err := sem.Acquire(ctx, 1); err != nil {
				return err
			}
			defer sem.Release(1)
			return ls.scanLayer(ctx, l, s)
		}
	}
	dedupe := make(map[string]struct{})
	for _, l := range layers {
		if _, ok := dedupe[l.Hash.String()]; ok {
			continue
		}
		dedupe[l.Hash.String()] = struct{}{}
		for _, s := range ls.ps {
			g.Go(launch(l, s))
		}
		for _, s := range ls.ds {
			g.Go(launch(l, s))
		}
		for _, s := range ls.rs {
			g.Go(launch(l, s))
		}
		for _, s := range ls.fis {
			g.Go(launch(l, s))
		}

	}

	return g.Wait()
}

// ScanLayer (along with the result type) handles an individual (scanner, layer)
// pair.
func (ls *LayerScanner) scanLayer(ctx context.Context, l *claircore.Layer, s VersionedScanner) error {
	ctx = zlog.ContextWithValues(ctx,
		"component", "indexer/LayerScanner.scanLayer",
		"scanner", s.Name(),
		"kind", s.Kind(),
		"layer", l.Hash.String())
	zlog.Debug(ctx).Msg("scan start")
	defer zlog.Debug(ctx).Msg("scan done")

	ok, err := ls.store.LayerScanned(ctx, l.Hash, s)
	if err != nil {
		return err
	}
	if ok {
		zlog.Debug(ctx).Msg("layer already scanned")
		return nil
	}

	var result result
	if err := result.Do(ctx, s, l); err != nil {
		return err
	}

	if err = ls.store.SetLayerScanned(ctx, l.Hash, s); err != nil {
		return fmt.Errorf("could not set layer scanned: %w", err)
	}

	return result.Store(ctx, ls.store, s, l)
}

// Result is a type that handles the kind-specific bits of the scan process.
type result struct {
	pkgs  []*claircore.Package
	dists []*claircore.Distribution
	repos []*claircore.Repository
	files []claircore.File
}

// Do asserts the Scanner back to having a Scan method, and then calls it.
//
// The success value is captured and the error value is returned by Do.
func (r *result) Do(ctx context.Context, s VersionedScanner, l *claircore.Layer) error {
	var err error
	switch s := s.(type) {
	case PackageScanner:
		r.pkgs, err = s.Scan(ctx, l)
	case DistributionScanner:
		r.dists, err = s.Scan(ctx, l)
	case RepositoryScanner:
		r.repos, err = s.Scan(ctx, l)
	case FileScanner:
		r.files, err = s.Scan(ctx, l)
	default:
		panic(fmt.Sprintf("programmer error: unknown type %T used as scanner", s))
	}
	addrErr := &net.AddrError{}
	switch {
	case errors.Is(err, nil):
	case errors.As(err, &addrErr):
		zlog.Warn(ctx).Str("scanner", s.Name()).Err(err).Msg("scanner not able to access resources")
		return nil
	default:
		zlog.Info(ctx).Err(err).Send()
	}

	return err
}

// Store calls the properly typed store method on whatever value was captured in
// the result.
func (r *result) Store(ctx context.Context, store Store, s VersionedScanner, l *claircore.Layer) error {
	switch {
	case r.pkgs != nil:
		zlog.Debug(ctx).Int("count", len(r.pkgs)).Msg("scan returned packages")
		return store.IndexPackages(ctx, r.pkgs, l, s)
	case r.dists != nil:
		zlog.Debug(ctx).Int("count", len(r.dists)).Msg("scan returned dists")
		return store.IndexDistributions(ctx, r.dists, l, s)
	case r.repos != nil:
		zlog.Debug(ctx).Int("count", len(r.repos)).Msg("scan returned repos")
		return store.IndexRepositories(ctx, r.repos, l, s)
	case r.files != nil:
		zlog.Debug(ctx).Int("count", len(r.files)).Msg("scan returned files")
		return store.IndexFiles(ctx, r.files, l, s)
	}
	zlog.Debug(ctx).Msg("scan returned a nil")
	return nil
}
