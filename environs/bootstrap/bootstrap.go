// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package bootstrap

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/utils"
	"github.com/juju/utils/series"
	"github.com/juju/utils/ssh"

	"github.com/juju/juju/cloudconfig/instancecfg"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/environs/storage"
	"github.com/juju/juju/environs/sync"
	"github.com/juju/juju/environs/tools"
	"github.com/juju/juju/network"
	coretools "github.com/juju/juju/tools"
	"github.com/juju/juju/version"
)

const noToolsMessage = `Juju cannot bootstrap because no tools are available for your model.
You may want to use the 'agent-metadata-url' configuration setting to specify the tools location.
`

var (
	logger = loggo.GetLogger("juju.environs.bootstrap")
)

// BootstrapParams holds the parameters for bootstrapping an environment.
type BootstrapParams struct {
	// EnvironConstraints are merged with the bootstrap constraints
	// to choose the initial instance, and will be stored in the new
	// environment's state.
	EnvironConstraints constraints.Value

	// BootstrapConstraints are used to choose the initial instance.
	// BootstrapConstraints does not affect the environment-level
	// constraints.
	BootstrapConstraints constraints.Value

	// BootstrapSeries, if specified, is the series to use for the
	// initial bootstrap machine.
	BootstrapSeries string

	// BootstrapImage, if specified, is the image ID to use for the
	// initial bootstrap machine.
	BootstrapImage string

	// Placement, if non-empty, holds an environment-specific placement
	// directive used to choose the initial instance.
	Placement string

	// UploadTools reports whether we should upload the local tools and
	// override the environment's specified agent-version.
	UploadTools bool

	// MetadataDir is an optional path to a local directory containing
	// tools and/or image metadata.
	MetadataDir string

	// AgentVersion, if set, determines the exact tools version that
	// will be used to start the Juju agents.
	AgentVersion *version.Number
}

// Bootstrap bootstraps the given environment. The supplied constraints are
// used to provision the instance, and are also set within the bootstrapped
// environment.
func Bootstrap(ctx environs.BootstrapContext, environ environs.Environ, args BootstrapParams) error {
	cfg := environ.Config()
	network.SetPreferIPv6(cfg.PreferIPv6())
	if secret := cfg.AdminSecret(); secret == "" {
		return errors.Errorf("model configuration has no admin-secret")
	}
	if authKeys := ssh.SplitAuthorisedKeys(cfg.AuthorizedKeys()); len(authKeys) == 0 {
		// Apparently this can never happen, so it's not tested. But, one day,
		// Config will act differently (it's pretty crazy that, AFAICT, the
		// authorized-keys are optional config settings... but it's impossible
		// to actually *create* a config without them)... and when it does,
		// we'll be here to catch this problem early.
		return errors.Errorf("model configuration has no authorized-keys")
	}
	if _, hasCACert := cfg.CACert(); !hasCACert {
		return errors.Errorf("model configuration has no ca-cert")
	}
	if _, hasCAKey := cfg.CAPrivateKey(); !hasCAKey {
		return errors.Errorf("model configuration has no ca-private-key")
	}

	// Set default tools metadata source, add image metadata source,
	// then verify constraints. Providers may rely on image metadata
	// for constraint validation.
	var customImageMetadata []*imagemetadata.ImageMetadata
	if args.MetadataDir != "" {
		var err error
		customImageMetadata, err = setPrivateMetadataSources(environ, args.MetadataDir)
		if err != nil {
			return err
		}
	}
	if err := validateConstraints(environ, args.EnvironConstraints); err != nil {
		return err
	}
	if err := validateConstraints(environ, args.BootstrapConstraints); err != nil {
		return err
	}

	constraintsValidator, err := environ.ConstraintsValidator()
	if err != nil {
		return err
	}
	bootstrapConstraints, err := constraintsValidator.Merge(
		args.EnvironConstraints, args.BootstrapConstraints,
	)
	if err != nil {
		return err
	}

	_, supportsNetworking := environs.SupportsNetworking(environ)

	var bootstrapSeries *string
	if args.BootstrapSeries != "" {
		bootstrapSeries = &args.BootstrapSeries
	}

	ctx.Infof("Bootstrapping model %q", cfg.Name())
	logger.Debugf("model %q supports service/machine networks: %v", cfg.Name(), supportsNetworking)
	disableNetworkManagement, _ := cfg.DisableNetworkManagement()
	logger.Debugf("network management by juju enabled: %v", !disableNetworkManagement)
	availableTools, err := findAvailableTools(
		environ, args.AgentVersion, bootstrapConstraints.Arch,
		bootstrapSeries, args.UploadTools,
	)
	if errors.IsNotFound(err) {
		return errors.New(noToolsMessage)
	} else if err != nil {
		return err
	}
	if lxcMTU, ok := cfg.LXCDefaultMTU(); ok {
		logger.Debugf("using MTU %v for all created LXC containers' network interfaces", lxcMTU)
	}

	imageMetadata, err := bootstrapImageMetadata(
		environ, availableTools,
		args.BootstrapImage,
		&customImageMetadata,
	)
	if err != nil {
		return errors.Trace(err)
	}

	// If we're uploading, we must override agent-version;
	// if we're not uploading, we want to ensure we have an
	// agent-version set anyway, to appease FinishInstanceConfig.
	// In the latter case, setBootstrapTools will later set
	// agent-version to the correct thing.
	agentVersion := version.Current
	if args.AgentVersion != nil {
		agentVersion = *args.AgentVersion
	}
	if cfg, err = cfg.Apply(map[string]interface{}{
		"agent-version": agentVersion.String(),
	}); err != nil {
		return err
	}
	if err = environ.SetConfig(cfg); err != nil {
		return err
	}

	ctx.Infof("Starting new instance for initial controller")
	result, err := environ.Bootstrap(ctx, environs.BootstrapParams{
		EnvironConstraints:   args.EnvironConstraints,
		BootstrapConstraints: args.BootstrapConstraints,
		Placement:            args.Placement,
		AvailableTools:       availableTools,
		ImageMetadata:        imageMetadata,
	})
	if err != nil {
		return err
	}

	matchingTools, err := availableTools.Match(coretools.Filter{
		Arch:   result.Arch,
		Series: result.Series,
	})
	if err != nil {
		return err
	}
	selectedTools, err := setBootstrapTools(environ, matchingTools)
	if err != nil {
		return err
	}
	if selectedTools.URL == "" {
		if !args.UploadTools {
			logger.Warningf("no prepackaged tools available")
		}
		ctx.Infof("Building tools to upload (%s)", selectedTools.Version)
		builtTools, err := sync.BuildToolsTarball(&selectedTools.Version.Number, cfg.AgentStream())
		if err != nil {
			return errors.Annotate(err, "cannot upload bootstrap tools")
		}
		defer os.RemoveAll(builtTools.Dir)
		filename := filepath.Join(builtTools.Dir, builtTools.StorageName)
		selectedTools.URL = fmt.Sprintf("file://%s", filename)
		selectedTools.Size = builtTools.Size
		selectedTools.SHA256 = builtTools.Sha256Hash
	}

	ctx.Infof("Installing Juju agent on bootstrap instance")
	publicKey, err := userPublicSigningKey()
	if err != nil {
		return err
	}
	instanceConfig, err := instancecfg.NewBootstrapInstanceConfig(
		args.BootstrapConstraints, args.EnvironConstraints, result.Series, publicKey,
	)
	if err != nil {
		return err
	}
	instanceConfig.Tools = selectedTools
	instanceConfig.CustomImageMetadata = customImageMetadata
	if err := result.Finalize(ctx, instanceConfig); err != nil {
		return err
	}
	ctx.Infof("Bootstrap agent installed")
	return nil
}

func userPublicSigningKey() (string, error) {
	signingKeyFile := os.Getenv("JUJU_STREAMS_PUBLICKEY_FILE")
	signingKey := ""
	if signingKeyFile != "" {
		path, err := utils.NormalizePath(signingKeyFile)
		if err != nil {
			return "", errors.Annotatef(err, "cannot expand key file path: %s", signingKeyFile)
		}
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return "", errors.Annotatef(err, "invalid public key file: %s", path)
		}
		signingKey = string(b)
	}
	return signingKey, nil
}

// bootstrapImageMetadata returns the image metadata to use for bootstrapping
// the given environment. If the environment provider does not make use of
// simplestreams, no metadata will be returned.
//
// If a bootstrap image ID is specified, image metadat will be synthesised
// using that image ID, and the architecture and series specified by the
// initiator. In addition, the custom image metadat that is saved into the
// state database will have the synthesised image metadata added to it.
func bootstrapImageMetadata(
	environ environs.Environ,
	availableTools coretools.List,
	bootstrapImageId string,
	customImageMetadata *[]*imagemetadata.ImageMetadata,
) ([]*imagemetadata.ImageMetadata, error) {

	hasRegion, ok := environ.(simplestreams.HasRegion)
	if !ok {
		if bootstrapImageId != "" {
			// We only support specifying image IDs for providers
			// that use simplestreams for now.
			return nil, errors.NotSupportedf(
				"specifying bootstrap image for %q provider",
				environ.Config().Type(),
			)
		}
		// No region, no metadata.
		return nil, nil
	}
	region, err := hasRegion.Region()
	if err != nil {
		return nil, errors.Trace(err)
	}

	if bootstrapImageId != "" {
		arches := availableTools.Arches()
		if len(arches) != 1 {
			return nil, errors.NotValidf("multiple architectures with bootstrap image")
		}
		allSeries := availableTools.AllSeries()
		if len(allSeries) != 1 {
			return nil, errors.NotValidf("multiple series with bootstrap image")
		}
		seriesVersion, err := series.SeriesVersion(allSeries[0])
		if err != nil {
			return nil, errors.Trace(err)
		}
		// The returned metadata does not have information about the
		// storage or virtualisation type. Any provider that wants to
		// filter on those properties should allow for empty values.
		meta := &imagemetadata.ImageMetadata{
			Id:         bootstrapImageId,
			Arch:       arches[0],
			Version:    seriesVersion,
			RegionName: region.Region,
			Endpoint:   region.Endpoint,
			Stream:     environ.Config().ImageStream(),
		}
		*customImageMetadata = append(*customImageMetadata, meta)
		return []*imagemetadata.ImageMetadata{meta}, nil
	}

	// For providers that support making use of simplestreams
	// image metadata, search public image metadata. We need
	// to pass this onto Bootstrap for selecting images.
	sources, err := environs.ImageMetadataSources(environ)
	if err != nil {
		return nil, errors.Trace(err)
	}
	imageConstraint := imagemetadata.NewImageConstraint(simplestreams.LookupParams{
		CloudSpec: region,
		Series:    availableTools.AllSeries(),
		Arches:    availableTools.Arches(),
		Stream:    environ.Config().ImageStream(),
	})
	publicImageMetadata, _, err := imagemetadata.Fetch(sources, imageConstraint)
	if err != nil {
		return nil, errors.Annotate(err, "searching image metadata")
	}
	return publicImageMetadata, nil
}

// setBootstrapTools returns the newest tools from the given tools list,
// and updates the agent-version configuration attribute.
func setBootstrapTools(environ environs.Environ, possibleTools coretools.List) (*coretools.Tools, error) {
	if len(possibleTools) == 0 {
		return nil, fmt.Errorf("no bootstrap tools available")
	}
	var newVersion version.Number
	newVersion, toolsList := possibleTools.Newest()
	logger.Infof("newest version: %s", newVersion)
	cfg := environ.Config()
	if agentVersion, _ := cfg.AgentVersion(); agentVersion != newVersion {
		cfg, err := cfg.Apply(map[string]interface{}{
			"agent-version": newVersion.String(),
		})
		if err == nil {
			err = environ.SetConfig(cfg)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to update model configuration: %v", err)
		}
	}
	bootstrapVersion := newVersion
	// We should only ever bootstrap the exact same version as the client,
	// or we risk bootstrap incompatibility. We still set agent-version to
	// the newest version, so the agent will immediately upgrade itself.
	if !isCompatibleVersion(newVersion, version.Current) {
		compatibleVersion, compatibleTools := findCompatibleTools(possibleTools, version.Current)
		if len(compatibleTools) == 0 {
			logger.Warningf(
				"failed to find %s tools, will attempt to use %s",
				version.Current, newVersion,
			)
		} else {
			bootstrapVersion, toolsList = compatibleVersion, compatibleTools
		}
	}
	logger.Infof("picked bootstrap tools version: %s", bootstrapVersion)
	return toolsList[0], nil
}

// findCompatibleTools finds tools in the list that have the same major, minor
// and patch level as version.Current.
//
// Build number is not important to match; uploaded tools will have
// incremented build number, and we want to match them.
func findCompatibleTools(possibleTools coretools.List, version version.Number) (version.Number, coretools.List) {
	var compatibleTools coretools.List
	for _, tools := range possibleTools {
		if isCompatibleVersion(tools.Version.Number, version) {
			compatibleTools = append(compatibleTools, tools)
		}
	}
	return compatibleTools.Newest()
}

func isCompatibleVersion(v1, v2 version.Number) bool {
	v1.Build = 0
	v2.Build = 0
	return v1.Compare(v2) == 0
}

// setPrivateMetadataSources sets the default tools metadata source
// for tools syncing, and adds an image metadata source after verifying
// the contents.
func setPrivateMetadataSources(env environs.Environ, metadataDir string) ([]*imagemetadata.ImageMetadata, error) {
	logger.Infof("Setting default tools and image metadata sources: %s", metadataDir)
	tools.DefaultBaseURL = metadataDir

	imageMetadataDir := filepath.Join(metadataDir, storage.BaseImagesPath)
	if _, err := os.Stat(imageMetadataDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Annotate(err, "cannot access image metadata")
		}
		return nil, nil
	}

	baseURL := fmt.Sprintf("file://%s", filepath.ToSlash(imageMetadataDir))
	datasource := simplestreams.NewURLDataSource("bootstrap metadata", baseURL, utils.NoVerifySSLHostnames, simplestreams.CUSTOM_CLOUD_DATA, false)

	// Read the image metadata, as we'll want to upload it to the environment.
	imageConstraint := imagemetadata.NewImageConstraint(simplestreams.LookupParams{})
	existingMetadata, _, err := imagemetadata.Fetch([]simplestreams.DataSource{datasource}, imageConstraint)
	if err != nil && !errors.IsNotFound(err) {
		return nil, errors.Annotate(err, "cannot read image metadata")
	}

	// Add an image metadata datasource for constraint validation, etc.
	environs.RegisterUserImageDataSourceFunc("bootstrap metadata", func(environs.Environ) (simplestreams.DataSource, error) {
		return datasource, nil
	})
	logger.Infof("custom image metadata added to search path")
	return existingMetadata, nil
}

func validateConstraints(env environs.Environ, cons constraints.Value) error {
	validator, err := env.ConstraintsValidator()
	if err != nil {
		return err
	}
	unsupported, err := validator.Validate(cons)
	if len(unsupported) > 0 {
		logger.Warningf("unsupported constraints: %v", unsupported)
	}
	return err
}

// EnsureNotBootstrapped returns nil if the environment is not
// bootstrapped, and an error if it is or if the function was not able
// to tell.
func EnsureNotBootstrapped(env environs.Environ) error {
	_, err := env.ControllerInstances()
	// If there is no error determining controller instaces,
	// then we are bootstrapped.
	switch errors.Cause(err) {
	case nil:
		return environs.ErrAlreadyBootstrapped
	case environs.ErrNoInstances:
		// TODO(axw) 2015-02-03 #1417526
		// We should not be relying on this result,
		// as it is possible for there to be no
		// controllers despite the environment
		// being bootstrapped.
		fallthrough
	case environs.ErrNotBootstrapped:
		return nil
	}
	return err
}
