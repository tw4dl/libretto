package apiversions

import (
	"strings"

	"github.com/apcera/libretto/Godeps/_workspace/src/github.com/rackspace/gophercloud"
)

func getURL(c *gophercloud.ServiceClient, version string) string {
	return c.ServiceURL(strings.TrimRight(version, "/") + "/")
}

func listURL(c *gophercloud.ServiceClient) string {
	return c.ServiceURL("")
}
