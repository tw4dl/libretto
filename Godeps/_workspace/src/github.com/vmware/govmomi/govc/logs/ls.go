/*
Copyright (c) 2015 VMware, Inc. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logs

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/apcera/libretto/Godeps/_workspace/src/golang.org/x/net/context"

	"github.com/apcera/libretto/Godeps/_workspace/src/github.com/vmware/govmomi/govc/cli"
	"github.com/apcera/libretto/Godeps/_workspace/src/github.com/vmware/govmomi/govc/flags"
	"github.com/apcera/libretto/Godeps/_workspace/src/github.com/vmware/govmomi/object"
)

type ls struct {
	*flags.HostSystemFlag
}

func init() {
	cli.Register("logs.ls", &ls{})
}

func (cmd *ls) Register(f *flag.FlagSet) {}

func (cmd *ls) Process() error { return nil }

func (cmd *ls) Run(f *flag.FlagSet) error {
	ctx := context.TODO()

	c, err := cmd.Client()
	if err != nil {
		return err
	}

	var host *object.HostSystem

	if c.ServiceContent.About.ApiType == "VirtualCenter" {
		host, err = cmd.HostSystemIfSpecified()
		if err != nil {
			return err
		}
	}

	m := object.NewDiagnosticManager(c)

	desc, err := m.QueryDescriptions(ctx, host)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)

	for _, d := range desc {
		fmt.Fprintf(tw, "%s\t%s\n", d.Key, d.FileName)
	}

	return tw.Flush()
}
