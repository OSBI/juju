// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package commands

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils/arch"
	jujuos "github.com/juju/utils/os"
	"github.com/juju/utils/series"
	gc "gopkg.in/check.v1"

	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/cmd/juju/block"
	"github.com/juju/juju/cmd/modelcmd"
	cmdtesting "github.com/juju/juju/cmd/testing"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/bootstrap"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/configstore"
	"github.com/juju/juju/environs/filestorage"
	"github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/simplestreams"
	sstesting "github.com/juju/juju/environs/simplestreams/testing"
	"github.com/juju/juju/environs/sync"
	envtesting "github.com/juju/juju/environs/testing"
	envtools "github.com/juju/juju/environs/tools"
	toolstesting "github.com/juju/juju/environs/tools/testing"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/juju"
	"github.com/juju/juju/juju/osenv"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/dummy"
	coretesting "github.com/juju/juju/testing"
	coretools "github.com/juju/juju/tools"
	"github.com/juju/juju/version"
)

type BootstrapSuite struct {
	coretesting.FakeJujuXDGDataHomeSuite
	testing.MgoSuite
	envtesting.ToolsFixture
	mockBlockClient *mockBlockClient

	modelFlags []string
}

var _ = gc.Suite(&BootstrapSuite{})

func (s *BootstrapSuite) SetUpSuite(c *gc.C) {
	s.FakeJujuXDGDataHomeSuite.SetUpSuite(c)
	s.MgoSuite.SetUpSuite(c)
	s.PatchValue(&simplestreams.SimplestreamsJujuPublicKey, sstesting.SignedMetadataPublicKey)
}

func (s *BootstrapSuite) SetUpTest(c *gc.C) {
	s.FakeJujuXDGDataHomeSuite.SetUpTest(c)
	s.MgoSuite.SetUpTest(c)
	s.ToolsFixture.SetUpTest(c)

	// Set version.Current to a known value, for which we
	// will make tools available. Individual tests may
	// override this.
	s.PatchValue(&version.Current, v100p64.Number)
	s.PatchValue(&arch.HostArch, func() string { return v100p64.Arch })
	s.PatchValue(&series.HostSeries, func() string { return v100p64.Series })
	s.PatchValue(&jujuos.HostOS, func() jujuos.OSType { return jujuos.Ubuntu })

	// Set up a local source with tools.
	sourceDir := createToolsSource(c, vAll)
	s.PatchValue(&envtools.DefaultBaseURL, sourceDir)

	s.PatchValue(&envtools.BundleTools, toolstesting.GetMockBundleTools(c))

	s.mockBlockClient = &mockBlockClient{}
	s.PatchValue(&blockAPI, func(c *modelcmd.ModelCommandBase) (block.BlockListAPI, error) {
		if s.mockBlockClient.discoveringSpacesError > 0 {
			s.mockBlockClient.discoveringSpacesError -= 1
			return nil, errors.New("space discovery still in progress")
		}
		return s.mockBlockClient, nil
	})

	s.modelFlags = []string{"-m", "--model"}
}

func (s *BootstrapSuite) TearDownSuite(c *gc.C) {
	s.MgoSuite.TearDownSuite(c)
	s.FakeJujuXDGDataHomeSuite.TearDownSuite(c)
}

func (s *BootstrapSuite) TearDownTest(c *gc.C) {
	s.ToolsFixture.TearDownTest(c)
	s.MgoSuite.TearDownTest(c)
	s.FakeJujuXDGDataHomeSuite.TearDownTest(c)
	dummy.Reset()
}

type mockBlockClient struct {
	retryCount             int
	numRetries             int
	discoveringSpacesError int
}

func (c *mockBlockClient) List() ([]params.Block, error) {
	c.retryCount += 1
	if c.retryCount == 5 {
		return nil, fmt.Errorf("upgrade in progress")
	}
	if c.numRetries < 0 {
		return nil, fmt.Errorf("other error")
	}
	if c.retryCount < c.numRetries {
		return nil, fmt.Errorf("upgrade in progress")
	}
	return []params.Block{}, nil
}

func (c *mockBlockClient) Close() error {
	return nil
}

func (s *BootstrapSuite) TestBootstrapAPIReadyRetries(c *gc.C) {
	s.PatchValue(&bootstrapReadyPollDelay, 1*time.Millisecond)
	s.PatchValue(&bootstrapReadyPollCount, 5)
	defaultSeriesVersion := version.Current
	// Force a dev version by having a non zero build number.
	// This is because we have not uploaded any tools and auto
	// upload is only enabled for dev versions.
	defaultSeriesVersion.Build = 1234
	s.PatchValue(&version.Current, defaultSeriesVersion)
	for _, t := range []struct {
		numRetries int
		err        string
	}{
		{0, ""},                    // agent ready immediately
		{2, ""},                    // agent ready after 2 polls
		{6, "upgrade in progress"}, // agent ready after 6 polls but that's too long
		{-1, "other error"},        // another error is returned
	} {
		for _, modelFlag := range s.modelFlags {

			resetJujuXDGDataHome(c, "devenv")

			s.mockBlockClient.numRetries = t.numRetries
			s.mockBlockClient.retryCount = 0
			_, err := coretesting.RunCommand(c, newBootstrapCommand(), modelFlag, "devenv", "--auto-upgrade")
			if t.err == "" {
				c.Check(err, jc.ErrorIsNil)
			} else {
				c.Check(err, gc.ErrorMatches, t.err)
			}
			expectedRetries := t.numRetries
			if t.numRetries <= 0 {
				expectedRetries = 1
			}
			// Only retry maximum of bootstrapReadyPollCount times.
			if expectedRetries > 5 {
				expectedRetries = 5
			}
			c.Check(s.mockBlockClient.retryCount, gc.Equals, expectedRetries)
		}
	}
}

func (s *BootstrapSuite) TestBootstrapAPIReadyWaitsForSpaceDiscovery(c *gc.C) {
	defaultSeriesVersion := version.Current
	// Force a dev version by having a non zero build number.
	// This is because we have not uploaded any tools and auto
	// upload is only enabled for dev versions.
	defaultSeriesVersion.Build = 1234
	s.PatchValue(&version.Current, defaultSeriesVersion)
	resetJujuXDGDataHome(c, "devenv")

	s.mockBlockClient.discoveringSpacesError = 2
	_, err := coretesting.RunCommand(c, newBootstrapCommand(), "-m", "devenv", "--auto-upgrade")
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(s.mockBlockClient.discoveringSpacesError, gc.Equals, 0)
}

func (s *BootstrapSuite) TestRunTests(c *gc.C) {
	for i, test := range bootstrapTests {
		c.Logf("\ntest %d: %s", i, test.info)
		restore := s.run(c, test)
		restore()
	}
}

type bootstrapTest struct {
	info string
	// binary version string used to set version.Current
	version string
	sync    bool
	args    []string
	err     string
	// binary version string for expected tools; if set, no default tools
	// will be uploaded before running the test.
	upload               string
	constraints          constraints.Value
	bootstrapConstraints constraints.Value
	placement            string
	hostArch             string
	keepBroken           bool
}

func (s *BootstrapSuite) patchVersionAndSeries(c *gc.C, envName string) {
	env := resetJujuXDGDataHome(c, envName)
	s.PatchValue(&series.HostSeries, func() string { return config.PreferredSeries(env.Config()) })
	s.patchVersion(c)
}

func (s *BootstrapSuite) patchVersion(c *gc.C) {
	// Force a dev version by having a non zero build number.
	// This is because we have not uploaded any tools and auto
	// upload is only enabled for dev versions.
	num := version.Current
	num.Build = 1234
	s.PatchValue(&version.Current, num)
}

func (s *BootstrapSuite) run(c *gc.C, test bootstrapTest) testing.Restorer {
	// Create home with dummy provider and remove all
	// of its envtools.
	env := resetJujuXDGDataHome(c, "peckham")

	// Although we're testing PrepareEndpointsForCaching interactions
	// separately in the juju package, here we just ensure it gets
	// called with the right arguments.
	prepareCalled := false
	addrConnectedTo := "localhost:17070"
	restore := testing.PatchValue(
		&prepareEndpointsForCaching,
		func(info configstore.EnvironInfo, hps [][]network.HostPort, addr network.HostPort) (_, _ []string, _ bool) {
			prepareCalled = true
			addrs, hosts, changed := juju.PrepareEndpointsForCaching(info, hps, addr)
			// Because we're bootstrapping the addresses will always
			// change, as there's no .jenv file saved yet.
			c.Assert(changed, jc.IsTrue)
			return addrs, hosts, changed
		},
	)

	if test.version != "" {
		useVersion := strings.Replace(test.version, "%LTS%", config.LatestLtsSeries(), 1)
		v := version.MustParseBinary(useVersion)
		restore = restore.Add(testing.PatchValue(&version.Current, v.Number))
		restore = restore.Add(testing.PatchValue(&arch.HostArch, func() string { return v.Arch }))
		restore = restore.Add(testing.PatchValue(&series.HostSeries, func() string { return v.Series }))
	}

	if test.hostArch != "" {
		restore = restore.Add(testing.PatchValue(&arch.HostArch, func() string { return test.hostArch }))
	}

	// Run command and check for uploads.
	opc, errc := cmdtesting.RunCommand(cmdtesting.NullContext(c), newBootstrapCommand(), test.args...)
	// Check for remaining operations/errors.
	if test.err != "" {
		err := <-errc
		c.Assert(err, gc.NotNil)
		stripped := strings.Replace(err.Error(), "\n", "", -1)
		c.Check(stripped, gc.Matches, test.err)
		return restore
	}
	if !c.Check(<-errc, gc.IsNil) {
		return restore
	}

	opBootstrap := (<-opc).(dummy.OpBootstrap)
	c.Check(opBootstrap.Env, gc.Equals, "peckham")
	c.Check(opBootstrap.Args.EnvironConstraints, gc.DeepEquals, test.constraints)
	if test.bootstrapConstraints == (constraints.Value{}) {
		test.bootstrapConstraints = test.constraints
	}
	c.Check(opBootstrap.Args.BootstrapConstraints, gc.DeepEquals, test.bootstrapConstraints)
	c.Check(opBootstrap.Args.Placement, gc.Equals, test.placement)

	opFinalizeBootstrap := (<-opc).(dummy.OpFinalizeBootstrap)
	c.Check(opFinalizeBootstrap.Env, gc.Equals, "peckham")
	c.Check(opFinalizeBootstrap.InstanceConfig.Tools, gc.NotNil)
	if test.upload != "" {
		c.Check(opFinalizeBootstrap.InstanceConfig.Tools.Version.String(), gc.Equals, test.upload)
	}

	store, err := configstore.Default()
	c.Assert(err, jc.ErrorIsNil)
	// Check a CA cert/key was generated by reloading the environment.
	env, err = environs.NewFromName("peckham", store)
	c.Assert(err, jc.ErrorIsNil)
	_, hasCert := env.Config().CACert()
	c.Check(hasCert, jc.IsTrue)
	_, hasKey := env.Config().CAPrivateKey()
	c.Check(hasKey, jc.IsTrue)
	info, err := store.ReadInfo("peckham")
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(info, gc.NotNil)
	c.Assert(prepareCalled, jc.IsTrue)
	c.Assert(info.APIEndpoint().Addresses, gc.DeepEquals, []string{addrConnectedTo})
	return restore
}

var bootstrapTests = []bootstrapTest{{
	info: "no args, no error, no upload, no constraints",
}, {
	info: "bad --constraints",
	args: []string{"--constraints", "bad=wrong"},
	err:  `invalid value "bad=wrong" for flag --constraints: unknown constraint "bad"`,
}, {
	info: "conflicting --constraints",
	args: []string{"--constraints", "instance-type=foo mem=4G"},
	err:  `ambiguous constraints: "instance-type" overlaps with "mem"`,
}, {
	info:    "bad model",
	version: "1.2.3-%LTS%-amd64",
	args:    []string{"-m", "brokenenv", "--auto-upgrade"},
	err:     `failed to bootstrap model: dummy.Bootstrap is broken`,
}, {
	info:        "constraints",
	args:        []string{"--constraints", "mem=4G cpu-cores=4"},
	constraints: constraints.MustParse("mem=4G cpu-cores=4"),
}, {
	info:                 "bootstrap and environ constraints",
	args:                 []string{"--constraints", "mem=4G cpu-cores=4", "--bootstrap-constraints", "mem=8G"},
	constraints:          constraints.MustParse("mem=4G cpu-cores=4"),
	bootstrapConstraints: constraints.MustParse("mem=8G cpu-cores=4"),
}, {
	info:        "unsupported constraint passed through but no error",
	args:        []string{"--constraints", "mem=4G cpu-cores=4 cpu-power=10"},
	constraints: constraints.MustParse("mem=4G cpu-cores=4 cpu-power=10"),
}, {
	info:        "--upload-tools uses arch from constraint if it matches current version",
	version:     "1.3.3-saucy-ppc64el",
	hostArch:    "ppc64el",
	args:        []string{"--upload-tools", "--constraints", "arch=ppc64el"},
	upload:      "1.3.3.1-raring-ppc64el", // from version.Current
	constraints: constraints.MustParse("arch=ppc64el"),
}, {
	info:     "--upload-tools rejects mismatched arch",
	version:  "1.3.3-saucy-amd64",
	hostArch: "amd64",
	args:     []string{"--upload-tools", "--constraints", "arch=ppc64el"},
	err:      `failed to bootstrap model: cannot build tools for "ppc64el" using a machine running on "amd64"`,
}, {
	info:     "--upload-tools rejects non-supported arch",
	version:  "1.3.3-saucy-mips64",
	hostArch: "mips64",
	args:     []string{"--upload-tools"},
	err:      `failed to bootstrap model: model "peckham" of type dummy does not support instances running on "mips64"`,
}, {
	info:     "--upload-tools always bumps build number",
	version:  "1.2.3.4-raring-amd64",
	hostArch: "amd64",
	args:     []string{"--upload-tools"},
	upload:   "1.2.3.5-raring-amd64",
}, {
	info:      "placement",
	args:      []string{"--to", "something"},
	placement: "something",
}, {
	info:       "keep broken",
	args:       []string{"--keep-broken"},
	keepBroken: true,
}, {
	info: "additional args",
	args: []string{"anything", "else"},
	err:  `unrecognized args: \["anything" "else"\]`,
}, {
	info: "--agent-version with --upload-tools",
	args: []string{"--agent-version", "1.1.0", "--upload-tools"},
	err:  `--agent-version and --upload-tools can't be used together`,
}, {
	info: "invalid --agent-version value",
	args: []string{"--agent-version", "foo"},
	err:  `invalid version "foo"`,
}, {
	info:    "agent-version doesn't match client version major",
	version: "1.3.3-saucy-ppc64el",
	args:    []string{"--agent-version", "2.3.0"},
	err:     `requested agent version major.minor mismatch`,
}, {
	info:    "agent-version doesn't match client version minor",
	version: "1.3.3-saucy-ppc64el",
	args:    []string{"--agent-version", "1.4.0"},
	err:     `requested agent version major.minor mismatch`,
}}

func (s *BootstrapSuite) TestRunModelNameMissing(c *gc.C) {
	s.PatchValue(&getModelName, func(*bootstrapCommand) string { return "" })

	_, err := coretesting.RunCommand(c, newBootstrapCommand())

	c.Check(err, gc.ErrorMatches, "the name of the model must be specified")
}

const provisionalEnvs = `
environments:
    devenv:
        type: dummy
    cloudsigma:
        type: cloudsigma
    vsphere:
        type: vsphere
`

func (s *BootstrapSuite) TestCheckProviderProvisional(c *gc.C) {
	coretesting.WriteEnvironments(c, provisionalEnvs)

	err := checkProviderType("devenv")
	c.Assert(err, jc.ErrorIsNil)

	for name, flag := range provisionalProviders {
		// vsphere is disabled for gccgo. See lp:1440940.
		if name == "vsphere" && runtime.Compiler == "gccgo" {
			continue
		}
		c.Logf(" - trying %q -", name)
		err := checkProviderType(name)
		c.Check(err, gc.ErrorMatches, ".* provider is provisional .* set JUJU_DEV_FEATURE_FLAGS=.*")

		err = os.Setenv(osenv.JujuFeatureFlagEnvKey, flag)
		c.Assert(err, jc.ErrorIsNil)
		err = checkProviderType(name)
		c.Check(err, jc.ErrorIsNil)
	}
}

func (s *BootstrapSuite) TestBootstrapTwice(c *gc.C) {
	const envName = "devenv"
	s.patchVersionAndSeries(c, envName)

	_, err := coretesting.RunCommand(c, newBootstrapCommand(), "-m", envName, "--auto-upgrade")
	c.Assert(err, jc.ErrorIsNil)

	_, err = coretesting.RunCommand(c, newBootstrapCommand(), "-m", envName, "--auto-upgrade")
	c.Assert(err, gc.ErrorMatches, "model is already bootstrapped")
}

func (s *BootstrapSuite) TestBootstrapSetsCurrentModel(c *gc.C) {
	const envName = "devenv"
	s.patchVersionAndSeries(c, envName)

	coretesting.WriteEnvironments(c, coretesting.MultipleEnvConfig)
	ctx, err := coretesting.RunCommand(c, newBootstrapCommand(), "-m", "devenv", "--auto-upgrade")
	c.Assert(coretesting.Stderr(ctx), jc.Contains, "-> devenv")
	currentEnv, err := modelcmd.ReadCurrentModel()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(currentEnv, gc.Equals, "devenv")
}

type mockBootstrapInstance struct {
	instance.Instance
}

func (*mockBootstrapInstance) Addresses() ([]network.Address, error) {
	return []network.Address{{Value: "localhost"}}, nil
}

func (s *BootstrapSuite) TestBootstrapPropagatesEnvErrors(c *gc.C) {
	//TODO(bogdanteleaga): fix this for windows once permissions are fixed
	if runtime.GOOS == "windows" {
		c.Skip("bug 1403084: this is very platform specific. When/if we will support windows state machine, this will probably be rewritten.")
	}

	const envName = "devenv"
	s.patchVersionAndSeries(c, envName)
	s.PatchValue(&environType, func(string) (string, error) { return "", nil })

	_, err := coretesting.RunCommand(c, newBootstrapCommand(), "-m", envName, "--auto-upgrade")
	c.Assert(err, jc.ErrorIsNil)

	// Change permissions on the jenv file to simulate some kind of
	// unexpected error when trying to read info from the environment
	jenvFile := testing.JujuXDGDataHomePath("models", "cache.yaml")
	err = os.Chmod(jenvFile, os.FileMode(0200))
	c.Assert(err, jc.ErrorIsNil)

	// The second bootstrap should fail b/c of the propogated error
	_, err = coretesting.RunCommand(c, newBootstrapCommand(), "-m", envName)
	c.Assert(err, gc.ErrorMatches, "there was an issue examining the model: .*")
}

func (s *BootstrapSuite) TestBootstrapCleansUpIfEnvironPrepFails(c *gc.C) {
	cleanupRan := false

	s.PatchValue(&environType, func(string) (string, error) { return "", nil })
	s.PatchValue(
		&environFromName,
		func(
			*cmd.Context,
			string,
			string,
			func(environs.Environ) error,
		) (environs.Environ, func(), error) {
			return nil, func() { cleanupRan = true }, fmt.Errorf("mock")
		},
	)

	ctx := coretesting.Context(c)
	_, errc := cmdtesting.RunCommand(ctx, newBootstrapCommand(), "-m", "peckham")
	c.Check(<-errc, gc.Not(gc.IsNil))
	c.Check(cleanupRan, jc.IsTrue)
}

func (s *BootstrapSuite) TestBootstrapFailToPrepareDiesGracefully(c *gc.C) {
	destroyedEnvRan := false
	destroyedInfoRan := false

	// Mock functions
	mockDestroyPreparedEnviron := func(
		*cmd.Context,
		environs.Environ,
		configstore.Storage,
		string,
	) {
		destroyedEnvRan = true
	}

	mockDestroyEnvInfo := func(
		ctx *cmd.Context,
		cfgName string,
		store configstore.Storage,
		action string,
	) {
		destroyedInfoRan = true
	}

	mockEnvironFromName := func(
		ctx *cmd.Context,
		envName string,
		action string,
		_ func(environs.Environ) error,
	) (environs.Environ, func(), error) {
		// Always show that the environment is bootstrapped.
		return environFromNameProductionFunc(
			ctx,
			envName,
			action,
			func(env environs.Environ) error {
				return environs.ErrAlreadyBootstrapped
			})
	}

	mockPrepare := func(
		string,
		environs.BootstrapContext,
		configstore.Storage,
	) (environs.Environ, error) {
		return nil, fmt.Errorf("mock-prepare")
	}

	// Simulation: prepare should fail and we should only clean up the
	// jenv file. Any existing environment should not be destroyed.
	s.PatchValue(&destroyPreparedEnviron, mockDestroyPreparedEnviron)
	s.PatchValue(&environType, func(string) (string, error) { return "", nil })
	s.PatchValue(&environFromName, mockEnvironFromName)
	s.PatchValue(&environs.PrepareFromName, mockPrepare)
	s.PatchValue(&destroyEnvInfo, mockDestroyEnvInfo)

	ctx := coretesting.Context(c)
	_, errc := cmdtesting.RunCommand(ctx, newBootstrapCommand(), "-m", "peckham")
	c.Check(<-errc, gc.ErrorMatches, ".*mock-prepare$")
	c.Check(destroyedEnvRan, jc.IsFalse)
	c.Check(destroyedInfoRan, jc.IsTrue)
}

func (s *BootstrapSuite) TestBootstrapJenvWarning(c *gc.C) {
	const envName = "devenv"
	s.patchVersionAndSeries(c, envName)

	store, err := configstore.Default()
	c.Assert(err, jc.ErrorIsNil)
	ctx := coretesting.Context(c)
	environs.PrepareFromName(envName, modelcmd.BootstrapContext(ctx), store)

	logger := "jenv.warning.test"
	var testWriter loggo.TestWriter
	loggo.RegisterWriter(logger, &testWriter, loggo.WARNING)
	defer loggo.RemoveWriter(logger)

	_, errc := cmdtesting.RunCommand(ctx, newBootstrapCommand(), "-m", envName, "--auto-upgrade")
	c.Assert(<-errc, gc.IsNil)
	c.Assert(testWriter.Log(), jc.LogMatches, []string{"ignoring environments.yaml: using bootstrap config in .*"})
}

func (s *BootstrapSuite) TestInvalidLocalSource(c *gc.C) {
	s.PatchValue(&version.Current, version.MustParse("1.2.0"))
	env := resetJujuXDGDataHome(c, "devenv")

	// Bootstrap the environment with an invalid source.
	// The command returns with an error.
	_, err := coretesting.RunCommand(c, newBootstrapCommand(), "--metadata-source", c.MkDir())
	c.Check(err, gc.ErrorMatches, `failed to bootstrap model: Juju cannot bootstrap because no tools are available for your model(.|\n)*`)

	// Now check that there are no tools available.
	_, err = envtools.FindTools(
		env, version.Current.Major, version.Current.Minor, "released", coretools.Filter{})
	c.Assert(err, gc.FitsTypeOf, errors.NotFoundf(""))
}

// createImageMetadata creates some image metadata in a local directory.
func createImageMetadata(c *gc.C) (string, []*imagemetadata.ImageMetadata) {
	// Generate some image metadata.
	im := []*imagemetadata.ImageMetadata{
		{
			Id:         "1234",
			Arch:       "amd64",
			Version:    "13.04",
			RegionName: "region",
			Endpoint:   "endpoint",
		},
	}
	cloudSpec := &simplestreams.CloudSpec{
		Region:   "region",
		Endpoint: "endpoint",
	}
	sourceDir := c.MkDir()
	sourceStor, err := filestorage.NewFileStorageWriter(sourceDir)
	c.Assert(err, jc.ErrorIsNil)
	err = imagemetadata.MergeAndWriteMetadata("raring", im, cloudSpec, sourceStor)
	c.Assert(err, jc.ErrorIsNil)
	return sourceDir, im
}

func (s *BootstrapSuite) TestBootstrapCalledWithMetadataDir(c *gc.C) {
	sourceDir, _ := createImageMetadata(c)
	resetJujuXDGDataHome(c, "devenv")

	var bootstrap fakeBootstrapFuncs
	s.PatchValue(&getBootstrapFuncs, func() BootstrapInterface {
		return &bootstrap
	})

	coretesting.RunCommand(
		c, newBootstrapCommand(),
		"--metadata-source", sourceDir, "--constraints", "mem=4G",
	)
	c.Assert(bootstrap.args.MetadataDir, gc.Equals, sourceDir)
}

func (s *BootstrapSuite) checkBootstrapWithVersion(c *gc.C, vers, expect string) {
	resetJujuXDGDataHome(c, "devenv")

	var bootstrap fakeBootstrapFuncs
	s.PatchValue(&getBootstrapFuncs, func() BootstrapInterface {
		return &bootstrap
	})

	num := version.Current
	num.Major = 2
	num.Minor = 3
	s.PatchValue(&version.Current, num)
	coretesting.RunCommand(
		c, newBootstrapCommand(),
		"--agent-version", vers,
	)
	c.Assert(bootstrap.args.AgentVersion, gc.NotNil)
	c.Assert(*bootstrap.args.AgentVersion, gc.Equals, version.MustParse(expect))
}

func (s *BootstrapSuite) TestBootstrapWithVersionNumber(c *gc.C) {
	s.checkBootstrapWithVersion(c, "2.3.4", "2.3.4")
}

func (s *BootstrapSuite) TestBootstrapWithBinaryVersionNumber(c *gc.C) {
	s.checkBootstrapWithVersion(c, "2.3.4-trusty-ppc64", "2.3.4")
}

func (s *BootstrapSuite) TestBootstrapWithAutoUpgrade(c *gc.C) {
	resetJujuXDGDataHome(c, "devenv")

	var bootstrap fakeBootstrapFuncs
	s.PatchValue(&getBootstrapFuncs, func() BootstrapInterface {
		return &bootstrap
	})
	coretesting.RunCommand(
		c, newBootstrapCommand(),
		"--auto-upgrade",
	)
	c.Assert(bootstrap.args.AgentVersion, gc.IsNil)
}

func (s *BootstrapSuite) TestAutoSyncLocalSource(c *gc.C) {
	sourceDir := createToolsSource(c, vAll)
	s.PatchValue(&version.Current, version.MustParse("1.2.0"))
	env := resetJujuXDGDataHome(c, "peckham")

	// Bootstrap the environment with the valid source.
	// The bootstrapping has to show no error, because the tools
	// are automatically synchronized.
	_, err := coretesting.RunCommand(c, newBootstrapCommand(), "--metadata-source", sourceDir)
	c.Assert(err, jc.ErrorIsNil)

	// Now check the available tools which are the 1.2.0 envtools.
	checkTools(c, env, v120All)
}

func (s *BootstrapSuite) setupAutoUploadTest(c *gc.C, vers, ser string) environs.Environ {
	s.PatchValue(&envtools.BundleTools, toolstesting.GetMockBundleTools(c))
	sourceDir := createToolsSource(c, vAll)
	s.PatchValue(&envtools.DefaultBaseURL, sourceDir)

	// Change the tools location to be the test location and also
	// the version and ensure their later restoring.
	// Set the current version to be something for which there are no tools
	// so we can test that an upload is forced.
	s.PatchValue(&version.Current, version.MustParse(vers))
	s.PatchValue(&series.HostSeries, func() string { return ser })

	// Create home with dummy provider and remove all
	// of its envtools.
	return resetJujuXDGDataHome(c, "devenv")
}

func (s *BootstrapSuite) TestAutoUploadAfterFailedSync(c *gc.C) {
	s.PatchValue(&series.HostSeries, func() string { return config.LatestLtsSeries() })
	s.setupAutoUploadTest(c, "1.7.3", "quantal")
	// Run command and check for that upload has been run for tools matching
	// the current juju version.
	opc, errc := cmdtesting.RunCommand(cmdtesting.NullContext(c), newBootstrapCommand(), "-m", "devenv", "--auto-upgrade")
	c.Assert(<-errc, gc.IsNil)
	c.Check((<-opc).(dummy.OpBootstrap).Env, gc.Equals, "devenv")
	icfg := (<-opc).(dummy.OpFinalizeBootstrap).InstanceConfig
	c.Assert(icfg, gc.NotNil)
	c.Assert(icfg.Tools.Version.String(), gc.Equals, "1.7.3.1-raring-"+arch.HostArch())
}

func (s *BootstrapSuite) TestAutoUploadOnlyForDev(c *gc.C) {
	s.setupAutoUploadTest(c, "1.8.3", "precise")
	_, errc := cmdtesting.RunCommand(cmdtesting.NullContext(c), newBootstrapCommand())
	err := <-errc
	c.Assert(err, gc.ErrorMatches,
		"failed to bootstrap model: Juju cannot bootstrap because no tools are available for your model(.|\n)*")
}

func (s *BootstrapSuite) TestMissingToolsError(c *gc.C) {
	s.setupAutoUploadTest(c, "1.8.3", "precise")

	_, err := coretesting.RunCommand(c, newBootstrapCommand())
	c.Assert(err, gc.ErrorMatches,
		"failed to bootstrap model: Juju cannot bootstrap because no tools are available for your model(.|\n)*")
}

func (s *BootstrapSuite) TestMissingToolsUploadFailedError(c *gc.C) {
	buildToolsTarballAlwaysFails := func(forceVersion *version.Number, stream string) (*sync.BuiltTools, error) {
		return nil, fmt.Errorf("an error")
	}

	s.setupAutoUploadTest(c, "1.7.3", "precise")
	s.PatchValue(&sync.BuildToolsTarball, buildToolsTarballAlwaysFails)

	ctx, err := coretesting.RunCommand(c, newBootstrapCommand(), "-m", "devenv", "--auto-upgrade")

	c.Check(coretesting.Stderr(ctx), gc.Equals, fmt.Sprintf(`
Bootstrapping model "devenv"
Starting new instance for initial controller
Building tools to upload (1.7.3.1-raring-%s)
`[1:], arch.HostArch()))
	c.Check(err, gc.ErrorMatches, "failed to bootstrap model: cannot upload bootstrap tools: an error")
}

func (s *BootstrapSuite) TestBootstrapDestroy(c *gc.C) {
	for _, modelFlag := range s.modelFlags {
		resetJujuXDGDataHome(c, "devenv")
		s.patchVersion(c)

		opc, errc := cmdtesting.RunCommand(cmdtesting.NullContext(c), newBootstrapCommand(), modelFlag, "brokenenv", "--auto-upgrade")
		err := <-errc
		c.Assert(err, gc.ErrorMatches, "failed to bootstrap model: dummy.Bootstrap is broken")
		var opDestroy *dummy.OpDestroy
		for opDestroy == nil {
			select {
			case op := <-opc:
				switch op := op.(type) {
				case dummy.OpDestroy:
					opDestroy = &op
				}
			default:
				c.Error("expected call to env.Destroy")
				return
			}
		}
		c.Assert(opDestroy.Error, gc.ErrorMatches, "dummy.Destroy is broken")
	}
}

func (s *BootstrapSuite) TestBootstrapKeepBroken(c *gc.C) {
	for _, modelFlag := range s.modelFlags {
		resetJujuXDGDataHome(c, "devenv")
		s.patchVersion(c)

		opc, errc := cmdtesting.RunCommand(cmdtesting.NullContext(c), newBootstrapCommand(), modelFlag, "brokenenv", "--keep-broken", "--auto-upgrade")
		err := <-errc
		c.Assert(err, gc.ErrorMatches, "failed to bootstrap model: dummy.Bootstrap is broken")
		done := false
		for !done {
			select {
			case op, ok := <-opc:
				if !ok {
					done = true
					break
				}
				switch op.(type) {
				case dummy.OpDestroy:
					c.Error("unexpected call to env.Destroy")
					break
				}
			default:
				break
			}
		}
	}
}

// createToolsSource writes the mock tools and metadata into a temporary
// directory and returns it.
func createToolsSource(c *gc.C, versions []version.Binary) string {
	versionStrings := make([]string, len(versions))
	for i, vers := range versions {
		versionStrings[i] = vers.String()
	}
	source := c.MkDir()
	toolstesting.MakeTools(c, source, "released", versionStrings)
	return source
}

// resetJujuXDGDataHome restores an new, clean Juju home environment without tools.
func resetJujuXDGDataHome(c *gc.C, envName string) environs.Environ {
	jenvDir := testing.JujuXDGDataHomePath("models")
	err := os.RemoveAll(jenvDir)
	c.Assert(err, jc.ErrorIsNil)
	coretesting.WriteEnvironments(c, modelConfig)
	dummy.Reset()
	store, err := configstore.Default()
	c.Assert(err, jc.ErrorIsNil)
	env, err := environs.PrepareFromName(envName, modelcmd.BootstrapContext(cmdtesting.NullContext(c)), store)
	c.Assert(err, jc.ErrorIsNil)
	return env
}

// checkTools check if the environment contains the passed envtools.
func checkTools(c *gc.C, env environs.Environ, expected []version.Binary) {
	list, err := envtools.FindTools(
		env, version.Current.Major, version.Current.Minor, "released", coretools.Filter{})
	c.Check(err, jc.ErrorIsNil)
	c.Logf("found: " + list.String())
	urls := list.URLs()
	c.Check(urls, gc.HasLen, len(expected))
}

var (
	v100d64 = version.MustParseBinary("1.0.0-raring-amd64")
	v100p64 = version.MustParseBinary("1.0.0-precise-amd64")
	v100q32 = version.MustParseBinary("1.0.0-quantal-i386")
	v100q64 = version.MustParseBinary("1.0.0-quantal-amd64")
	v120d64 = version.MustParseBinary("1.2.0-raring-amd64")
	v120p64 = version.MustParseBinary("1.2.0-precise-amd64")
	v120q32 = version.MustParseBinary("1.2.0-quantal-i386")
	v120q64 = version.MustParseBinary("1.2.0-quantal-amd64")
	v120t32 = version.MustParseBinary("1.2.0-trusty-i386")
	v120t64 = version.MustParseBinary("1.2.0-trusty-amd64")
	v190p32 = version.MustParseBinary("1.9.0-precise-i386")
	v190q64 = version.MustParseBinary("1.9.0-quantal-amd64")
	v200p64 = version.MustParseBinary("2.0.0-precise-amd64")
	v100All = []version.Binary{
		v100d64, v100p64, v100q64, v100q32,
	}
	v120All = []version.Binary{
		v120d64, v120p64, v120q64, v120q32, v120t32, v120t64,
	}
	v190All = []version.Binary{
		v190p32, v190q64,
	}
	v200All = []version.Binary{
		v200p64,
	}
	vAll = joinBinaryVersions(v100All, v120All, v190All, v200All)
)

func joinBinaryVersions(versions ...[]version.Binary) []version.Binary {
	var all []version.Binary
	for _, versions := range versions {
		all = append(all, versions...)
	}
	return all
}

// TODO(menn0): This fake BootstrapInterface implementation is
// currently quite minimal but could be easily extended to cover more
// test scenarios. This could help improve some of the tests in this
// file which execute large amounts of external functionality.
type fakeBootstrapFuncs struct {
	args bootstrap.BootstrapParams
}

func (fake *fakeBootstrapFuncs) EnsureNotBootstrapped(env environs.Environ) error {
	return nil
}

func (fake *fakeBootstrapFuncs) Bootstrap(ctx environs.BootstrapContext, env environs.Environ, args bootstrap.BootstrapParams) error {
	fake.args = args
	return nil
}
