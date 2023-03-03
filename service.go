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

func RegisterServices(client *consul.Client, serviceName string, count int, flapInterval time.Duration, serviceTags string, mesh bool, stats chan Stat) error {
	log.Printf("Registering %d %s instances...\n", count, serviceName)

	checksTTL := flapInterval * 3
	if checksTTL == 0 {
		checksTTL = 10 * time.Hour
	}

	var tags []string
	if serviceTags != "" {
		tags = strings.Split(serviceTags, ",")
	}

	for instanceID := 0; instanceID < count; instanceID++ {
		id := fmt.Sprintf("%s-%d", serviceName, instanceID)
		if !mesh {
			err := client.Agent().ServiceRegister(&consul.AgentServiceRegistration{
				Name: serviceName,
				ID:   id,
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
		} else {
			err := RegisterProxy(client, serviceName, id, instanceID, tags, checksTTL)
			if err != nil {
				return err
			}
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

func RegisterProxy(client *consul.Client, serviceName string, serviceID string, id int, tags []string, checksTTL time.Duration) error {
	registration := &consul.AgentServiceRegistration{
		Kind:    consul.ServiceKindConnectProxy,
		Name:    fmt.Sprintf("%s", serviceID),
		ID:      fmt.Sprintf("%s", serviceID),
		Address: "127.0.0.1",
		Port:    9999,
		Tags:    tags,
		Checks: []*consul.AgentServiceCheck{
			{
				CheckID:                        fmt.Sprintf("check-%d", id),
				TTL:                            checksTTL.String(),
				Status:                         consul.HealthCritical,
				DeregisterCriticalServiceAfter: checksTTL.String(),
				SuccessBeforePassing:           1,
			},
		},
	}

	registration.Proxy = &consul.AgentServiceConnectProxyConfig{
		DestinationServiceName: fmt.Sprintf("%s-mesh-proxy", serviceID),
		DestinationServiceID:   fmt.Sprintf("%s-mesh-proxy", serviceID),
		LocalServiceAddress:    "127.0.0.1",
		LocalServicePort:       8500,
		//
	}

	return client.Agent().ServiceRegisterOpts(registration, consul.ServiceRegisterOpts{ReplaceExistingChecks: true})
}
