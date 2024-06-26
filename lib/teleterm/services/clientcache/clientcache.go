// Teleport
// Copyright (C) 2024 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package clientcache

import (
	"context"
	"slices"
	"sync"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/teleterm/api/uri"
	"github.com/gravitational/teleport/lib/teleterm/clusters"
)

// Cache stores clients keyed by cluster URI.
// Safe for concurrent access.
// Closes all clients and wipes the cache on Clear.
type Cache struct {
	cfg Config
	mu  sync.Mutex
	// clients keep mapping between cluster URI
	// (both root and leaf) and cluster clients
	clients map[uri.ResourceURI]*client.ClusterClient
	// group prevents duplicate requests to create clients
	// for a given cluster URI
	group singleflight.Group
}

type ResolveClusterFunc func(uri uri.ResourceURI) (*clusters.Cluster, *client.TeleportClient, error)

// Config describes the client cache configuration.
type Config struct {
	ResolveClusterFunc ResolveClusterFunc
	Log                logrus.FieldLogger
}

func (c *Config) checkAndSetDefaults() {
	if c.Log == nil {
		c.Log = logrus.WithField(teleport.ComponentKey, "clientcache")
	}
}

// New creates an instance of Cache.
func New(c Config) *Cache {
	c.checkAndSetDefaults()

	return &Cache{
		cfg:     c,
		clients: make(map[uri.ResourceURI]*client.ClusterClient),
	}
}

// Get returns a client from the cache if there is one,
// otherwise it dials the remote server.
// The caller should not close the returned client.
func (c *Cache) Get(ctx context.Context, clusterURI uri.ResourceURI) (*client.ClusterClient, error) {
	groupClt, err, _ := c.group.Do(clusterURI.String(), func() (any, error) {
		if fromCache := c.getFromCache(clusterURI); fromCache != nil {
			c.cfg.Log.WithField("cluster", clusterURI.String()).Info("Retrieved client from cache.")
			return fromCache, nil
		}

		_, clusterClient, err := c.cfg.ResolveClusterFunc(clusterURI)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		var newClient *client.ClusterClient
		if err := clusters.AddMetadataToRetryableError(ctx, func() error {
			clt, err := clusterClient.ConnectToCluster(ctx)
			if err != nil {
				return trace.Wrap(err)
			}
			newClient = clt
			return nil
		}); err != nil {
			return nil, trace.Wrap(err)
		}

		// We'll save the client in the cache, so we don't have to
		// build a new connection next time.
		// All cached clients will be closed when the daemon exits.
		c.addToCache(clusterURI, newClient)

		c.cfg.Log.WithField("cluster", clusterURI.String()).Info("Added client to cache.")

		return newClient, nil
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	clt, ok := groupClt.(*client.ClusterClient)
	if !ok {
		return nil, trace.BadParameter("unexpected type %T received for cluster client", groupClt)
	}

	return clt, nil
}

// ClearForRoot closes and removes clients from the cache
// for the root cluster and its leaf clusters.
func (c *Cache) ClearForRoot(clusterURI uri.ResourceURI) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	rootClusterURI := clusterURI.GetRootClusterURI()
	var (
		errors  []error
		deleted []string
	)

	for resourceURI, clt := range c.clients {
		if resourceURI.GetRootClusterURI() == rootClusterURI {
			if err := clt.Close(); err != nil {
				errors = append(errors, err)
			}
			deleted = append(deleted, resourceURI.GetClusterURI().String())
			delete(c.clients, resourceURI)
		}
	}

	c.cfg.Log.WithFields(
		logrus.Fields{"cluster": rootClusterURI.String(), "clients": deleted},
	).Info("Invalidated cached clients for root cluster.")

	return trace.NewAggregate(errors...)

}

// Clear closes and removes all clients.
func (c *Cache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errors []error
	for _, clt := range c.clients {
		if err := clt.Close(); err != nil {
			errors = append(errors, err)
		}
	}
	clear(c.clients)

	return trace.NewAggregate(errors...)
}

func (c *Cache) addToCache(clusterURI uri.ResourceURI, clusterClient *client.ClusterClient) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.clients[clusterURI] = clusterClient
}

func (c *Cache) getFromCache(clusterURI uri.ResourceURI) *client.ClusterClient {
	c.mu.Lock()
	defer c.mu.Unlock()

	clt := c.clients[clusterURI]
	return clt
}

// NoCache is a client cache implementation that returns a new client
// on each call to Get.
//
// ClearForRoot and Clear still work as expected.
type NoCache struct {
	mu                 sync.Mutex
	resolveClusterFunc ResolveClusterFunc
	clients            []noCacheClient
}

type noCacheClient struct {
	uri    uri.ResourceURI
	client *client.ClusterClient
}

func NewNoCache(resolveClusterFunc ResolveClusterFunc) *NoCache {
	return &NoCache{
		resolveClusterFunc: resolveClusterFunc,
	}
}

func (c *NoCache) Get(ctx context.Context, clusterURI uri.ResourceURI) (*client.ClusterClient, error) {
	_, clusterClient, err := c.resolveClusterFunc(clusterURI)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	newClient, err := clusterClient.ConnectToCluster(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	c.mu.Lock()
	c.clients = append(c.clients, noCacheClient{
		uri:    clusterURI,
		client: newClient,
	})
	c.mu.Unlock()

	return newClient, nil
}

func (c *NoCache) ClearForRoot(clusterURI uri.ResourceURI) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	rootClusterURI := clusterURI.GetRootClusterURI()
	var (
		errors []error
	)

	c.clients = slices.DeleteFunc(c.clients, func(ncc noCacheClient) bool {
		belongsToCluster := ncc.uri.GetRootClusterURI() == rootClusterURI

		if belongsToCluster {
			if err := ncc.client.Close(); err != nil {
				errors = append(errors, err)
			}
		}

		return belongsToCluster
	})

	return trace.NewAggregate(errors...)
}

func (c *NoCache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errors []error
	for _, ncc := range c.clients {
		if err := ncc.client.Close(); err != nil {
			errors = append(errors, err)
		}
	}
	c.clients = nil

	return trace.NewAggregate(errors...)
}
