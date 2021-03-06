// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package commands

import (
	"fmt"
	"strings"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"launchpad.net/gnuflag"

	"github.com/juju/juju/cmd/modelcmd"
	"github.com/juju/juju/environs/configstore"
)

func newAPIInfoCommand() cmd.Command {
	return modelcmd.Wrap(&apiInfoCommand{})
}

// apiInfoCommand returns the fields used to connect to an API server.
type apiInfoCommand struct {
	modelcmd.ModelCommandBase
	out       cmd.Output
	refresh   bool
	user      bool
	password  bool
	cacert    bool
	servers   bool
	modelUUID bool
	srvuuid   bool
	fields    []string
}

const apiInfoDoc = `
Print the field values used to connect to the model's API servers"

The exact fields to output can be specified on the command line.  The
available fields are:
  user
  password
  environ-uuid
  controllers
  ca-cert

If "password" is included as a field, or the --password option is given, the
password value will be shown.


Examples:
  $ juju api-info
  user: admin
  environ-uuid: 373b309b-4a86-4f13-88e2-c213d97075b8
  controllers:
  - localhost:17070
  - 10.0.3.1:17070
  - 192.168.2.21:17070
  ca-cert: '-----BEGIN CERTIFICATE-----
    ...
    -----END CERTIFICATE-----
  '

  $ juju api-info user
  admin

  $ juju api-info user password
  user: admin
  password: sekrit


`

func (c *apiInfoCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "api-info",
		Args:    "[field ...]",
		Purpose: "print the field values used to connect to the model's API servers",
		Doc:     apiInfoDoc,
	}
}

func (c *apiInfoCommand) Init(args []string) error {
	c.fields = args
	if len(args) == 0 {
		c.user = true
		c.modelUUID = true
		c.srvuuid = true
		c.servers = true
		c.cacert = true
		return nil
	}

	var unknown []string
	for _, name := range args {
		switch name {
		case "user":
			c.user = true
		case "password":
			c.password = true
		case "environ-uuid":
			c.modelUUID = true
		case "controllers":
			c.servers = true
		case "ca-cert":
			c.cacert = true
		case "server-uuid":
			c.srvuuid = true
		default:
			unknown = append(unknown, fmt.Sprintf("%q", name))
		}
	}
	if len(unknown) > 0 {
		return errors.Errorf("unknown fields: %s", strings.Join(unknown, ", "))
	}

	return nil
}

func (c *apiInfoCommand) SetFlags(f *gnuflag.FlagSet) {
	c.out.AddFlags(f, "default", map[string]cmd.Formatter{
		"default": c.format,
		"yaml":    cmd.FormatYaml,
		"json":    cmd.FormatJson,
	})
	f.BoolVar(&c.refresh, "refresh", false, "connect to the API to ensure an up-to-date endpoint location")
	f.BoolVar(&c.password, "password", false, "include the password in the output fields")
}

func connectionEndpoint(c modelcmd.ModelCommandBase, refresh bool) (configstore.APIEndpoint, error) {
	return c.ConnectionEndpoint(refresh)
}

func connectionCredentials(c modelcmd.ModelCommandBase) (configstore.APICredentials, error) {
	return c.ConnectionCredentials()
}

var (
	endpoint = connectionEndpoint
	creds    = connectionCredentials
)

// Print out the addresses of the API server endpoints.
func (c *apiInfoCommand) Run(ctx *cmd.Context) error {
	apiendpoint, err := endpoint(c.ModelCommandBase, c.refresh)
	if err != nil {
		return err
	}
	credentials, err := creds(c.ModelCommandBase)
	if err != nil {
		return err
	}

	var result InfoData
	if c.user {
		result.User = credentials.User
	}
	if c.password {
		result.Password = credentials.Password
	}
	if c.modelUUID {
		result.ModelUUID = apiendpoint.ModelUUID
	}
	if c.servers {
		result.Controllers = apiendpoint.Addresses
	}
	if c.cacert {
		result.CACert = apiendpoint.CACert
	}
	if c.srvuuid {
		result.ServerUUID = apiendpoint.ServerUUID
	}

	return c.out.Write(ctx, result)
}

func (c *apiInfoCommand) format(value interface{}) ([]byte, error) {
	if len(c.fields) == 1 {
		data := value.(InfoData)
		field, err := data.field(c.fields[0])
		if err != nil {
			return nil, err
		}
		switch value := field.(type) {
		case []string:
			return []byte(strings.Join(value, "\n")), nil
		case string:
			return []byte(value), nil
		default:
			return nil, errors.Errorf("Unsupported type %T", field)
		}
	}

	return cmd.FormatYaml(value)
}

type InfoData struct {
	User        string   `json:"user,omitempty" yaml:",omitempty"`
	Password    string   `json:"password,omitempty" yaml:",omitempty"`
	ModelUUID   string   `json:"environ-uuid,omitempty" yaml:"environ-uuid,omitempty"`
	ServerUUID  string   `json:"server-uuid,omitempty" yaml:"server-uuid,omitempty"`
	Controllers []string `json:"controllers,omitempty" yaml:"controllers,omitempty"`
	CACert      string   `json:"ca-cert,omitempty" yaml:"ca-cert,omitempty"`
}

func (i *InfoData) field(name string) (interface{}, error) {
	switch name {
	case "user":
		return i.User, nil
	case "password":
		return i.Password, nil
	case "environ-uuid":
		return i.ModelUUID, nil
	case "controllers":
		return i.Controllers, nil
	case "ca-cert":
		return i.CACert, nil
	case "server-uuid":
		return i.ServerUUID, nil
	default:
		return "", errors.Errorf("unknown field %q", name)
	}
}
