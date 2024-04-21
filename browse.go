package dnssd

import (
	"time"

	"github.com/brutella/dnssd/log"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/miekg/dns"

	"context"
	"fmt"
	"net"
)

// BrowseEntry represents a discovered service instance.
type BrowseEntry struct {
	IPs       []net.IP
	Host      string
	Port      int
	IfaceName string
	Name      string
	Type      string
	Domain    string
	Text      map[string]string
	TTL       time.Duration
}

// AddFunc is called when a service instance was found.
type AddFunc func(BrowseEntry)

// RmvFunc is called when a service instance disappared.
type RmvFunc func(BrowseEntry)

// LookupType browses for service instanced with a specified service type.
func LookupType(ctx context.Context, service string, add AddFunc, rmv RmvFunc) (err error) {
	conn, err := newMDNSConn()
	if err != nil {
		return err
	}
	defer conn.close()

	return lookupTypes(ctx, []string{service}, conn, add, rmv)
}

func LookupTypes(ctx context.Context, services []string, add AddFunc, rmv RmvFunc) (err error) {
	conn, err := newMDNSConn()
	if err != nil {
		return err
	}
	defer conn.close()

	return lookupTypes(ctx, services, conn, add, rmv)
}

// ServiceInstanceName returns the service instance name
// in the form of <instance name>.<service>.<domain>.
// (Note the trailing dot.)
func (e BrowseEntry) ServiceInstanceName() string {
	return fmt.Sprintf("%s.%s.%s.", e.Name, e.Type, e.Domain)
}

// UnescapedServiceInstanceName returns the same as `ServiceInstanceName()`
// but removes any escape characters.
func (e BrowseEntry) UnescapedServiceInstanceName() string {
	return fmt.Sprintf("%s.%s.%s.", e.UnescapedName(), e.Type, e.Domain)
}

// UnescapedName returns the unescaped instance name.
func (e BrowseEntry) UnescapedName() string {
	return unquote.Replace(e.Name)
}

func lookupTypes(ctx context.Context, services []string, conn MDNSConn, add AddFunc, rmv RmvFunc) (err error) {
	var cache = NewCache()

	m := new(dns.Msg)
	m.Question = []dns.Question{}
	for _, s := range services {
		m.Question = append(m.Question, dns.Question{
			Name:   s,
			Qtype:  dns.TypePTR,
			Qclass: dns.ClassINET,
		})
	}
	// TODO include known answers which current ttl is more than half of the correct ttl (see TFC6772 7.1: Known-Answer Supression)
	// m.Answer = ...
	// m.Authoritive = false // because our answers are *believes*

	readCtx, readCancel := context.WithCancel(ctx)
	defer readCancel()

	ch := conn.Read(readCtx)

	qs := make(chan *Query)
	go func() {
		for _, iface := range MulticastInterfaces() {
			iface := iface
			q := &Query{msg: m, iface: iface}
			qs <- q
		}
	}()

	serviceSet := mapset.NewSet[string](services...)
	es := []*BrowseEntry{}
	for {
		select {
		case q := <-qs:
			log.Debug.Printf("Send browsing query at %s\n%s\n", q.IfaceName(), q.msg)
			if err := conn.SendQuery(q); err != nil {
				log.Debug.Println("SendQuery:", err)
			}

		case req := <-ch:
			log.Debug.Printf("Receive message at %s\n%s\n", req.IfaceName(), req.msg)
			cache.UpdateFrom(req.msg, req.iface)
			for _, srv := range cache.Services() {
				if !serviceSet.ContainsOne(srv.ServiceName()) {
					continue
				}

				for ifaceName, ips := range srv.ifaceIPs {
					var found = false
					for _, e := range es {
						if e.Type == srv.Type && e.Name == srv.Name && e.IfaceName == ifaceName {
							found = true
							break
						}
					}
					if !found {
						e := BrowseEntry{
							IPs:       ips,
							Host:      srv.Host,
							Port:      srv.Port,
							IfaceName: ifaceName,
							Name:      srv.Name,
							Type:      srv.Type,
							Domain:    srv.Domain,
							Text:      srv.Text,
							TTL:       srv.TTL,
						}
						es = append(es, &e)
						add(e)
					}
				}
			}

			tmp := []*BrowseEntry{}
			for _, e := range es {
				var found = false
				for _, srv := range cache.Services() {
					if srv.ServiceInstanceName() == e.ServiceInstanceName() {
						found = true
						break
					}
				}

				if found {
					tmp = append(tmp, e)
				} else {
					// TODO
					rmv(*e)
				}
			}
			es = tmp
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
