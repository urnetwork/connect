// Default extender DNS discovery publishes the first usable family promptly,
// retains later inventory, and joins every background request with its client.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	extenderBootstrapDnsPublication = "bootstrap"
	extenderManualDnsPublication    = "manual"
)

// A single bounded bootstrap pass or manual-host generation. first closes only
// after directory publication or terminal failure; done closes after all work.
// firstErr is immutable once first closes. All other fields are constructor data.
type extenderDnsPublication struct {
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	first      chan struct{}
	done       chan struct{}
	firstErr   error
}

// Registers asynchronous settings work before Close can join its owner.
func (self *ExtenderNetworkClient) startDnsWorker() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.dnsClosed || self.ctx.Err() != nil {
		return false
	}
	self.dnsWorkers.Add(1)
	return true
}

// At most one bootstrap and one manual generation resolve at a time. A repeated
// refresh reuses the active pass; replacement cancels and joins its predecessor.
// The deadline is copied from the foreground pass, not restarted on handoff.
func (self *ExtenderNetworkClient) startDnsPublication(
	kind string,
	generation uint64,
	deadline time.Time,
	work func(context.Context, func(bool)) error,
) *extenderDnsPublication {
	for {
		self.stateLock.Lock()
		if self.dnsClosed || self.ctx.Err() != nil ||
			kind == extenderManualDnsPublication && generation != self.manualHostsVersion {
			self.stateLock.Unlock()
			return nil
		}
		if previous := self.dnsPublicationKVs[kind]; previous != nil {
			if previous.generation == generation {
				self.stateLock.Unlock()
				return previous
			}
			self.stateLock.Unlock()
			previous.cancel()
			select {
			case <-previous.done:
				continue
			case <-self.ctx.Done():
				return nil
			}
		}
		ctx, cancel := context.WithDeadline(self.ctx, deadline)
		publication := &extenderDnsPublication{
			generation: generation,
			ctx:        ctx,
			cancel:     cancel,
			first:      make(chan struct{}),
			done:       make(chan struct{}),
		}
		if self.dnsPublicationKVs == nil {
			self.dnsPublicationKVs = map[string]*extenderDnsPublication{}
		}
		self.dnsPublicationKVs[kind] = publication
		self.dnsWorkers.Add(1)
		self.stateLock.Unlock()

		go HandleError(func() {
			published := false
			workerErr := errors.New("extender DNS publication interrupted")
			defer func() {
				cancel()
				if !published {
					publication.firstErr = workerErr
					close(publication.first)
				}
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					if self.dnsPublicationKVs[kind] == publication {
						delete(self.dnsPublicationKVs, kind)
					}
				}()
				close(publication.done)
				self.dnsWorkers.Done()
			}()
			workerErr = work(ctx, func(changed bool) {
				if !published {
					published = true
					close(publication.first)
				}
				if changed {
					self.probeWake.NotifyAll()
					// A previous candidate may have let the foreground leave
					// before this first answer. Every actual addition wakes it;
					// unchanged cache refreshes must preserve feed backoff.
					self.wakeMonitor.NotifyAll()
				}
			})
		})
		return publication
	}
}

// A foreground pass waits for readiness, not completion of the sibling query.
func (self *extenderDnsPublication) waitFirst(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-self.first:
		return self.firstErr
	}
}

// Signed TXT records have already been verified before this starts. Unsigned
// A/AAAA entries retain their DNS source and cannot become feed-dial authority.
func (self *ExtenderNetworkClient) bootstrapDnsAddresses(ctx context.Context) {
	deadline, _ := ctx.Deadline()
	publication := self.startDnsPublication(extenderBootstrapDnsPublication, 0, deadline, func(ctx context.Context, ready func(bool)) error {
		return self.resolveDnsPublication(ctx, self.settings.ExtenderDnsName, func(ips []netip.Addr) {
			changed := false
			for _, ip := range ips {
				changed = self.directory.AddBootstrap(ip, ExtenderSourceDns) || changed
			}
			ready(changed)
		})
	})
	if publication == nil {
		return
	}
	// A valid TXT or previously trusted/manual candidate is already usable.
	// DNS address refresh cannot hold its first feed dial behind another family.
	if 0 < len(self.candidates()) {
		self.probeWake.NotifyAll()
		return
	}
	if err := publication.waitFirst(self.ctx); err != nil && self.ctx.Err() == nil {
		self.log.Infof("[extender]bootstrap err = %s\n", err)
	}
}

// Manual hosts are one serial batch, so at most two DNS family queries are
// live for it. The first published address releases the foreground; later
// hosts and the sibling family remain owned until completion or cancellation.
func (self *ExtenderNetworkClient) applyManualHostsProgressive() uint64 {
	hosts, version := self.manualHostsValue()
	deadline := time.Now().Add(self.settings.HelloTimeout)
	publication := self.startDnsPublication(extenderManualDnsPublication, version, deadline, func(ctx context.Context, ready func(bool)) error {
		for _, host := range hosts {
			if err := ctx.Err(); err != nil {
				return err
			}
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			if ip, err := netip.ParseAddr(host); err == nil {
				ready(self.directory.AddManual(ip))
				continue
			}
			err := self.resolveDnsPublication(ctx, host, func(ips []netip.Addr) {
				changed := false
				for _, ip := range ips {
					changed = self.directory.AddManual(ip) || changed
				}
				ready(changed)
			})
			if err != nil && ctx.Err() == nil {
				self.log.Infof("[extender]manual host %s err = %s\n", host, err)
			}
		}
		return ctx.Err()
	})
	if publication == nil || 0 < len(self.candidates()) {
		return version
	}
	_ = publication.waitFirst(self.ctx)
	return version
}

// A caller-supplied whole-list resolver keeps its contract. Only the default
// DoH implementation can publish separate family batches before completion.
func (self *ExtenderNetworkClient) resolveDnsPublication(
	ctx context.Context,
	name string,
	publish func([]netip.Addr),
) error {
	if resolve := self.settings.ResolveDns; resolve != nil {
		ips, err := resolve(ctx, name)
		if err == nil && 0 < len(ips) && ctx.Err() == nil {
			publish(ips)
		}
		return err
	}
	_, err := self.resolveDnsProgress(ctx, name, publish)
	return err
}

// Concurrent permitted families publish in arrival order, without dropping
// later inventory. Ordinary resolver fallback occurs only when DoH yields no
// address, exactly as before. This call joins both one-shot query owners.
func (self *ExtenderNetworkClient) resolveDnsProgress(
	ctx context.Context,
	name string,
	publish func([]netip.Addr),
) ([]netip.Addr, error) {
	dohSettings := self.settings.DohSettings
	if dohSettings == nil && self.clientStrategy != nil {
		dohSettings = self.clientStrategy.settings.DohSettings
	}
	var ips []netip.Addr
	seen := map[netip.Addr]bool{}
	accept := func(addrs []netip.Addr) {
		var fresh []netip.Addr
		for _, ip := range addrs {
			ip = ip.Unmap()
			if ip.IsValid() && !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
				fresh = append(fresh, ip)
			}
		}
		if 0 < len(fresh) && publish != nil && ctx.Err() == nil {
			publish(fresh)
		}
	}
	if dohSettings != nil {
		queryCtx, cancel := context.WithCancel(ctx)
		results := make(chan []netip.Addr, 2)
		var workers sync.WaitGroup
		defer func() {
			cancel()
			workers.Wait()
		}()
		pending := 0
		for _, query := range []struct {
			ipVersion  int
			recordType string
		}{
			{ipVersion: 4, recordType: "A"},
			{ipVersion: 6, recordType: "AAAA"},
		} {
			if !self.ipVersionSupported(query.ipVersion) {
				continue
			}
			pending++
			workers.Add(1)
			go func() {
				defer workers.Done()
				var addrs []netip.Addr
				for ip := range DohQuery(queryCtx, query.ipVersion, query.recordType, dohSettings, name) {
					addrs = append(addrs, ip)
				}
				results <- orderDialAddrs(addrs)
			}()
		}
		for 0 < pending {
			select {
			case <-ctx.Done():
				if 0 < len(ips) {
					return ips, nil
				}
				return ips, ctx.Err()
			case addrs := <-results:
				pending--
				accept(addrs)
			}
		}
	}
	if 0 < len(ips) {
		return ips, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var customResolver *net.Resolver
	if self.clientStrategy != nil {
		customResolver = self.clientStrategy.settings.ConnectSettings.Resolver
	}
	netIps, err := dialResolver(customResolver).LookupNetIP(ctx, "ip", name)
	if err != nil {
		return nil, err
	}
	accept(netIps)
	return ips, nil
}
