package statecmd_test
import (
	stdtesting "testing"
	coretesting "launchpad.net/juju-core/testing"
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/juju/testing"
	"launchpad.net/juju-core/state/statecmd"
)

type ConfigSuite struct {
	testing.JujuConnSuite
}

func Test(t *stdtesting.T) {
	coretesting.MgoTestPackage(t)
}

var _ = Suite(&ConfigSuite{})

// TODO(rog) make these tests independent of one another.
var setTests = []struct {
	about string
	params statecmd.SetConfigParams
	expect map[string]interface{} // resulting configuration of the dummy service.
	err      string                 // error regex
}{
	{
		about: "unknown service name",
		params: statecmd.SetConfigParams{
			ServiceName: "unknown-service",
			Options: map[string]string{
				"foo": "bar",
			},
		},
		err: `service "unknown-service" not found`,
	}, {
		about: "no config or options",
		params: statecmd.SetConfigParams{},
		err: "no options to set",
	}, {
		about: "bad configuration",
		params: statecmd.SetConfigParams{
			Config: "345",
		},
		err: "no options to set",
	}, {
		about: "config with no options",
		params: statecmd.SetConfigParams{
			Config: "{}",
		},
		err: "no options to set",
	},  {
		about: "unknown option",
		params: statecmd.SetConfigParams{
			ServiceName: "dummy-service",
			Options: map[string]string{
				"foo": "bar",
			},
		},
		err: `Unknown configuration option: "foo"`,
	}, {
		about: "set outlook",
		params: statecmd.SetConfigParams{
			ServiceName: "dummy-service",
			Options: map[string]string{
				"outlook": "positive",
			},
		},
		expect: map[string]interface{}{
			"outlook": "positive",
		},
	}, {
		about: "unset outlook and set title",
		params: statecmd.SetConfigParams{
			ServiceName: "dummy-service",
			Options: map[string]string{
				"outlook": "",
				"title": "sir",
			},
		},
		expect: map[string]interface{}{
			"title": "sir",
		},
	}, {
		about: "set a default value",
		params: statecmd.SetConfigParams{
			ServiceName: "dummy-service",
			Options: map[string]string{
				"username": "admin001",
			},
		},
		expect: map[string]interface{}{
			"username": "admin001",
			"title":    "sir",
		},
	}, {
		about: "unset a default value, set a different default",
		params: statecmd.SetConfigParams{
			ServiceName: "dummy-service",
			Options: map[string]string{
				"username": "",
				"title": "My Title",
			},
		},
		expect: map[string]interface{}{
			"title": "My Title",
		},
	}, {
		about: "yaml config",
		params: statecmd.SetConfigParams{
			ServiceName: "dummy-service",
			Config: "skill-level: 9000\nusername: admin001\n\n",
		},
		expect: map[string]interface{}{
			"title":       "My Title",
			"username":    "admin001",
			"skill-level": int64(9000), // yaml int types are int64
		},
	},
}

func (s *ConfigSuite) TestSetConfig(c *C) {
	sch := s.AddTestingCharm(c, "dummy")
	svc, err := s.State.AddService("dummy-service", sch)
	c.Assert(err, IsNil)
	for i, t := range setTests {
		c.Logf("test %d. %s", i, t.about)
		err = statecmd.SetConfig(s.State, t.params)
		if t.err != "" {
			c.Check(err, ErrorMatches, t.err)
		} else {
			c.Assert(err, IsNil)
			cfg, err := svc.Config()
			c.Assert(err, IsNil)
			c.Assert(cfg.Map(), DeepEquals, t.expect)
		}
	}
}
