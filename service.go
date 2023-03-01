package main

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	consul "github.com/hashicorp/consul/api"
)

func DeregisterServices(client *consul.Client, serviceName string) error {
	log.Printf("Deregistering service %s...", serviceName)

	services, err := client.Agent().Services()
	if err != nil {
		return err
	}

	for _, s := range services {
		if s.Service != serviceName {
			continue
		}

		log.Printf("Deregistering %s", s.ID)
		err := client.Agent().ServiceDeregister(s.ID)
		if err != nil {
			return err
		}
	}

	return nil
}

func RegisterServices(client *consul.Client, serviceName string, count int, flapInterval time.Duration, serviceTags string, stats chan Stat) error {
	log.Printf("Registering %d %s instances...\n", count, serviceName)

	checksTTL := flapInterval * 3
	if checksTTL == 0 {
		checksTTL = 10 * time.Minute
	}

	var tags []string
	if serviceTags != "" {
		tags = strings.Split(serviceTags, ",")
	}

	for instanceID := 0; instanceID < count; instanceID++ {
		err := client.Agent().ServiceRegister(&consul.AgentServiceRegistration{
			Name: serviceName,
			ID:   fmt.Sprintf("%s-%d", serviceName, instanceID),
			Checks: []*consul.AgentServiceCheck{
				{
					CheckID:                        fmt.Sprintf("check-%d", instanceID),
					TTL:                            checksTTL.String(),
					Status:                         consul.HealthCritical,
					DeregisterCriticalServiceAfter: checksTTL.String(),
				},
			},
			Tags: tags,
		})
		if err != nil {
			return err
		}

		err = RegisterProxy(client, &consulProxy{
			name:            serviceName,
			id:              fmt.Sprintf("%s-%d-proxy", serviceName, instanceID),
			ip:              "127.0.0.1",
			port:            123,
			destinationName: serviceName,
			destinationID:   fmt.Sprintf("%s-%d-proxy", serviceName, instanceID),
			localServiceIP:  "127.0.01",
		})
		if err != nil {
			return err
		}

	}

	flapping := flapInterval > 0

	if flapping {
		log.Printf("Flapping instances every %s", flapInterval)
	}

	waitTime := flapInterval
	if waitTime <= 0 {
		waitTime = checksTTL / 2
	}

	var fps int32

	log.Println("Retrieving checks states")
	checks, err := client.Agent().Checks()
	if err != nil {
		return err
	}

	for instanceID := 0; instanceID < count; instanceID++ {
		go func(instanceID int) {
			time.Sleep((flapInterval / time.Duration(count)) * time.Duration(instanceID))
			client.Agent().Checks()

			var lastStatus bool
			checkName := fmt.Sprintf("check-%d", instanceID)
			check, ok := checks[checkName]
			if !ok {
				log.Printf("could not find check %s", checkName)
			} else {
				lastStatus = check.Status == consul.HealthPassing
			}
			for {
				var f func(checkID, note string) error

				// flap check if flapping is enabled, else just keep check alive
				if lastStatus && flapping {
					f = client.Agent().FailTTL
				} else {
					f = client.Agent().PassTTL
				}

				err := f(fmt.Sprintf("check-%d", instanceID), "")
				if err != nil {
					log.Fatal(err)
				}
				lastStatus = !lastStatus

				if flapping {
					atomic.AddInt32(&fps, 1)
				}

				time.Sleep(waitTime)
			}
		}(instanceID)
	}
	go func() {
		for range time.Tick(time.Second) {
			f := atomic.SwapInt32(&fps, 0)
			stats <- Stat{"FPS", float64(f)}
		}
	}()

	log.Println("Services registered")

	return nil
}

// consulProxy is a proxy service registered into a local Consul agent. In this
// case, the proxy service represents an Envoy proxy running alongside an
// underlying (destination) service.
type consulProxy struct {
	name string
	id   string
	ip   string
	port int
	tags []string
	meta map[string]string

	// desinationName is the name of the underlying service.
	destinationName string
	// destinationID is the ID of the underlying service.
	destinationID string
	// localServiceIP is the IP where the underlying service can be reached.
	localServiceIP string
	// localServicePort is the port on localhost where the underlying service can
	// be reached.
	localServicePort int
	// healthPort is the port number that the proxy serves a /health endpoint.
	healthPort int
	// protocol is the protocol the underlying service uses (tcp, http, http2, or
	// grpc).
	protocol string
	// timeout allows sets the local_request_timeout_ms field when registering
	// the proxy to override the default 15s timeout that consul sets
	timeout int
	// upstreams is a list of upstream remote services that can be reached via
	// this proxy. In another context, these might be called dependent services
	// or dependencies.
	upstreams []consulUpstream
}

// consulUpstream is a remote service that can be reached via the local proxy.
type consulUpstream struct {
	// destinationName is the name of the upstream service.
	destinationName string
	// port is the port on localhost where a local service can connect to reach
	// it via the proxy.
	port int
	// protocol is the protocol the upstream service uses (tcp, http, http2, or
	// grpc).
	protocol string
}

func RegisterProxy(client *consul.Client, proxy *consulProxy) error {
	registration := &consul.AgentServiceRegistration{
		Kind:    consul.ServiceKindConnectProxy,
		Name:    proxy.name,
		ID:      proxy.id,
		Address: proxy.ip,
		Port:    proxy.port,
		Tags:    proxy.tags,
		Meta:    proxy.meta,

		Checks: consul.AgentServiceChecks{
			/*			{
							Name:                           "TCP reachability",
							TCP:                            fmt.Sprintf("%s:%d", proxy.ip, proxy.port),
							Interval:                       (10 * time.Second).String(),
							DeregisterCriticalServiceAfter: (10 * time.Minute).String(),
						},
						{
							// Require a passing healthcheck from envoy-monitor.
							Name:     "Monitor health",
							HTTP:     fmt.Sprintf("http://%s:%d%s", proxy.ip, proxy.healthPort, healthPath),
							Interval: (10 * time.Second).String(),
						},
						{
							// The proxy service will only be healthy if the underlying service is
							// also healthy
							Name:         "Destination alias",
							AliasService: proxy.destinationID,
						},*/
		},
	}

	registration.Proxy = &consul.AgentServiceConnectProxyConfig{
		DestinationServiceName: proxy.destinationName,
		DestinationServiceID:   proxy.destinationID,
		LocalServiceAddress:    proxy.localServiceIP,
		LocalServicePort:       proxy.localServicePort,
		Config: map[string]interface{}{
			"protocol": proxy.protocol,
		},
		/*	Expose: consul.ExposeConfig{
			Checks: true,
		},*/
	}

	/*
		if !strings.EqualFold(proxy.protocol, "tcp") && proxy.timeout > -1 { // 0 is a valid value which disables timeouts explicitly
			registration.Proxy.Config["local_request_timeout_ms"] = proxy.timeout
		}*/
	/*
		for _, upstream := range proxy.upstreams {
			registration.Proxy.Upstreams = append(registration.Proxy.Upstreams, consulapi.Upstream{
				DestinationType:  consulapi.UpstreamDestTypeService,
				DestinationName:  upstream.destinationName,
				LocalBindAddress: "127.0.0.1",
				LocalBindPort:    upstream.port,
				Config: map[string]interface{}{
					"protocol": upstream.protocol,
				},
			})
		}
	*/
	return client.Agent().ServiceRegisterOpts(registration, consul.ServiceRegisterOpts{ReplaceExistingChecks: true})
}
