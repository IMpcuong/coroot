package collector

import (
	"errors"
	"net/http"
	"sort"

	"golang.org/x/exp/maps"

	"github.com/coroot/coroot/constructor"
	"github.com/coroot/coroot/db"
	"github.com/coroot/coroot/model"
	"github.com/coroot/coroot/timeseries"
	"github.com/coroot/coroot/utils"
	"inet.af/netaddr"
	"k8s.io/klog"
)

func (c *Collector) Config(w http.ResponseWriter, r *http.Request) {
	projectId := db.ProjectId(r.Header.Get(ApiKeyHeader))
	project, err := c.getProject(projectId)
	if err != nil {
		klog.Errorln(err)
		if errors.Is(err, ErrProjectNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	cacheClient := c.cache.GetCacheClient(project.Id)
	cacheTo, err := cacheClient.GetTo()
	if err != nil {
		klog.Errorln(err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if cacheTo.IsZero() {
		return
	}
	to := cacheTo
	from := to.Add(-timeseries.Hour)
	step, err := cacheClient.GetStep(from, to)
	if err != nil {
		klog.Errorln(err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	ctr := constructor.New(c.db, project, cacheClient, nil)
	world, err := ctr.LoadWorld(r.Context(), from, to, step, nil)
	if err != nil {
		klog.Errorln(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var res model.Config

	res.AWSConfig = project.Settings.Integrations.AWS

	for _, app := range world.Applications {
		instancesByType := map[model.ApplicationType]map[*model.Instance]bool{}
		if app.Id.Kind == model.ApplicationKindExternalService {
			for _, d := range app.Downstreams {
				if d.RemoteInstance == nil || d.RemoteInstance.IsObsolete() {
					continue
				}
				for protocol := range d.RequestsCount {
					t := protocol.ToApplicationType()
					if t == model.ApplicationTypeUnknown {
						continue
					}
					if instancesByType[t] == nil {
						instancesByType[t] = map[*model.Instance]bool{}
					}
					instancesByType[t][d.RemoteInstance] = true
				}
			}
		} else {
			for _, instance := range app.Instances {
				if instance.IsObsolete() {
					continue
				}
				for t := range instance.ApplicationTypes() {
					if instancesByType[t] == nil {
						instancesByType[t] = map[*model.Instance]bool{}
					}
					instancesByType[t][instance] = true
				}
			}
		}

		for t := range app.ApplicationTypes() {
			var instrumentation *model.ApplicationInstrumentation
			if app.Settings != nil && app.Settings.Instrumentation != nil && app.Settings.Instrumentation[t] != nil {
				instrumentation = app.Settings.Instrumentation[t]
			} else {
				instrumentation = model.GetDefaultInstrumentation(t)
			}
			if instrumentation == nil || instrumentation.Disabled {
				continue
			}
			switch instrumentation.Type {
			case model.ApplicationTypePostgres, model.ApplicationTypeMysql:
				if instrumentation.Credentials.Username == "" || instrumentation.Credentials.Password == "" {
					continue
				}
			}
			for instance := range instancesByType[t] {
				ips := map[string]netaddr.IP{}
				for listen, active := range instance.TcpListens {
					if active && listen.Port == instrumentation.Port {
						if ip, err := netaddr.ParseIP(listen.IP); err == nil {
							ips[listen.IP] = ip
						}
					}
				}
				if ip := SelectIP(maps.Values(ips)); ip != nil {
					i := *instrumentation // copy
					i.Host = ip.String()
					res.ApplicationInstrumentation = append(res.ApplicationInstrumentation, i)
				}
			}
		}
	}

	utils.WriteJson(w, res)
}

func SelectIP(ips []netaddr.IP) *netaddr.IP {
	if len(ips) == 0 {
		return nil
	}

	if len(ips) == 1 {
		return &ips[0]
	}

	type weightedIp struct {
		ip     netaddr.IP
		weight int
	}

	weightedIps := make([]weightedIp, 0, len(ips))
	for _, ip := range ips {
		rank := 5
		switch {
		case ip.IsLoopback():
			rank = 10
		case utils.IsIpDocker(ip):
			rank = 9
		case utils.IsIpPrivate(ip):
			rank = 1
			if ip.Is6() {
				rank = 2
			}
		case ip.Is6():
			rank = 6
		}
		weightedIps = append(weightedIps, weightedIp{ip, rank})
	}

	sort.Slice(weightedIps, func(i, j int) bool {
		ip1, ip2 := weightedIps[i], weightedIps[j]
		if ip1.weight == ip2.weight {
			return ip1.ip.String() < ip2.ip.String()
		}
		return ip1.weight < ip2.weight
	})

	return &weightedIps[0].ip
}
