// Stub provider for OpenStack, using goose will be implemented here

package openstack

import (
	"errors"
	"fmt"
	"io/ioutil"
	"launchpad.net/goose/client"
	gooseerrors "launchpad.net/goose/errors"
	"launchpad.net/goose/identity"
	"launchpad.net/goose/nova"
	"launchpad.net/goose/swift"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/cloudinit"
	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/log"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
	"launchpad.net/juju-core/trivial"
	"launchpad.net/juju-core/version"
	"net/http"
	"strings"
	"sync"
	"time"
)

const mgoPort = 37017
const apiPort = 17070

var mgoPortSuffix = fmt.Sprintf(":%d", mgoPort)
var apiPortSuffix = fmt.Sprintf(":%d", apiPort)

type environProvider struct{}

var _ environs.EnvironProvider = (*environProvider)(nil)

var providerInstance environProvider

// A request may fail to due "eventual consistency" semantics, which
// should resolve fairly quickly.  A request may also fail due to a slow
// state transition (for instance an instance taking a while to release
// a security group after termination).  The former failure mode is
// dealt with by shortAttempt, the latter by longAttempt.
var shortAttempt = trivial.AttemptStrategy{
	Total: 10 * time.Second, // it seems Nova needs more time than EC2
	Delay: 200 * time.Millisecond,
}

var longAttempt = trivial.AttemptStrategy{
	Total: 3 * time.Minute,
	Delay: 1 * time.Second,
}

func init() {
	environs.RegisterProvider("openstack", environProvider{})
}

func (p environProvider) BoilerplateConfig() string {
	return `
## https://juju.ubuntu.com/get-started/openstack/
openstack:
  type: openstack
  admin-secret: {{rand}}
  # Globally unique swift bucket name
  control-bucket: juju-{{rand}}
  # Usually set via the env variable OS_AUTH_URL, but can be specified here
  # auth-url: https://yourkeystoneurl:443/v2.0/
  # override if your workstation is running a different series to which you are deploying
  # default-series: precise
  default-image-id: c876e5fe-abb0-41f0-8f29-f0b47481f523
  # The following are used for userpass authentication (the default)
  auth-mode: userpass
  # Usually set via the env variable OS_USERNAME, but can be specified here
  # username: <your username>
  # Usually set via the env variable OS_PASSWORD, but can be specified here
  # password: <secret>
  # Usually set via the env variable OS_TENANT_NAME, but can be specified here
  # tenant-name: <your tenant name>
  # Usually set via the env variable OS_REGION_NAME, but can be specified here
  # region: <your region>

## https://juju.ubuntu.com/get-started/hp-cloud/
hpcloud:
  type: openstack
  admin-secret: {{rand}}
  # Globally unique swift bucket name
  control-bucket: juju-{{rand}}
  # Not required if env variable OS_AUTH_URL is set
  auth-url: https://yourkeystoneurl:35357/v2.0/
  # override if your workstation is running a different series to which you are deploying
  # default-series: precise
  default-image-id: 75845
  # The following are used for userpass authentication (the default)
  auth-mode: userpass
  # Usually set via the env variable OS_USERNAME, but can be specified here
  # username: <your username>
  # Usually set via the env variable OS_PASSWORD, but can be specified here
  # password: <secret>
  # Usually set via the env variable OS_TENANT_NAME, but can be specified here
  # tenant-name: <your tenant name>
  # Usually set via the env variable OS_REGION_NAME, but can be specified here
  # region: <your region>
  # The following are used for keypair authentication
  # auth-mode: keypair
  # Usually set via the env variable AWS_ACCESS_KEY_ID, but can be specified here
  # access-key: <secret>
  # Usually set via the env variable AWS_SECRET_ACCESS_KEY, but can be specified here
  # secret-key: <secret>

`[1:]
}

func (p environProvider) Open(cfg *config.Config) (environs.Environ, error) {
	log.Printf("environs/openstack: opening environment %q", cfg.Name())
	e := new(environ)
	err := e.SetConfig(cfg)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (p environProvider) SecretAttrs(cfg *config.Config) (map[string]interface{}, error) {
	m := make(map[string]interface{})
	ecfg, err := providerInstance.newConfig(cfg)
	if err != nil {
		return nil, err
	}
	m["username"] = ecfg.username()
	m["password"] = ecfg.password()
	m["tenant-name"] = ecfg.tenantName()
	return m, nil
}

func (p environProvider) PublicAddress() (string, error) {
	return fetchMetadata("public-hostname")
}

func (p environProvider) PrivateAddress() (string, error) {
	return fetchMetadata("local-hostname")
}

type environ struct {
	name string

	ecfgMutex             sync.Mutex
	ecfgUnlocked          *environConfig
	novaUnlocked          *nova.Client
	storageUnlocked       environs.Storage
	publicStorageUnlocked environs.Storage // optional.
}

var _ environs.Environ = (*environ)(nil)

type instance struct {
	e *environ
	*nova.ServerDetail
	address string
}

func (inst *instance) String() string {
	return inst.ServerDetail.Id
}

var _ environs.Instance = (*instance)(nil)

func (inst *instance) Id() state.InstanceId {
	return state.InstanceId(inst.ServerDetail.Id)
}

// instanceAddress processes a map of networks to lists of IP
// addresses, as returned by Nova.GetServer(), extracting the proper
// public (or private, if public is not available) IPv4 address, and
// returning it, or an error.
func instanceAddress(addresses map[string][]nova.IPAddress) (string, error) {
	var private, public, privateNet string
	for network, ips := range addresses {
		for _, address := range ips {
			if address.Version == 4 {
				if network == "public" {
					public = address.Address
				} else {
					privateNet = network
					// Some setups use custom network name, treat as "private"
					private = address.Address
				}
				break
			}
		}
	}
	// HP cloud/canonistack specific: public address is 2nd in the private network
	if prv, ok := addresses[privateNet]; public == "" && ok {
		if len(prv) > 1 && prv[1].Version == 4 {
			public = prv[1].Address
		}
	}
	// Juju assumes it always needs a public address and loops waiting for one.
	// In fact a private address is generally fine provided it can be sshed to.
	// (ported from py-juju/providers/openstack)
	if public == "" && private != "" {
		public = private
	}
	if public == "" {
		return "", environs.ErrNoDNSName
	}
	return public, nil
}

func (inst *instance) DNSName() (string, error) {
	if inst.address != "" {
		return inst.address, nil
	}
	// Fetch the instance information again, in case
	// the addresses have become available.
	server, err := inst.e.nova().GetServer(string(inst.Id()))
	if err != nil {
		return "", err
	}
	inst.address, err = instanceAddress(server.Addresses)
	if err != nil {
		return "", err
	}
	return inst.address, nil
}

func (inst *instance) WaitDNSName() (string, error) {
	for a := longAttempt.Start(); a.Next(); {
		addr, err := inst.DNSName()
		if err == nil || err != environs.ErrNoDNSName {
			return addr, err
		}
	}
	return "", fmt.Errorf("timed out trying to get DNS address for %v", inst.Id())
}

func (inst *instance) OpenPorts(machineId string, ports []state.Port) error {
	panic("OpenPorts not implemented")
}

func (inst *instance) ClosePorts(machineId string, ports []state.Port) error {
	panic("ClosePorts not implemented")
}

func (inst *instance) Ports(machineId string) ([]state.Port, error) {
	panic("Ports not implemented")
}

func (e *environ) ecfg() *environConfig {
	e.ecfgMutex.Lock()
	ecfg := e.ecfgUnlocked
	e.ecfgMutex.Unlock()
	return ecfg
}

func (e *environ) nova() *nova.Client {
	e.ecfgMutex.Lock()
	nova := e.novaUnlocked
	e.ecfgMutex.Unlock()
	return nova
}

func (e *environ) Name() string {
	return e.name
}

func (e *environ) Storage() environs.Storage {
	e.ecfgMutex.Lock()
	storage := e.storageUnlocked
	e.ecfgMutex.Unlock()
	return storage
}

func (e *environ) PublicStorage() environs.StorageReader {
	e.ecfgMutex.Lock()
	defer e.ecfgMutex.Unlock()
	if e.publicStorageUnlocked == nil {
		return environs.EmptyStorage
	}
	return e.publicStorageUnlocked
}

func (e *environ) Bootstrap(uploadTools bool, cert, key []byte) error {
	password := e.Config().AdminSecret()
	if password == "" {
		return fmt.Errorf("admin-secret is required for bootstrap")
	}
	log.Printf("environs/openstack: bootstrapping environment %q", e.name)
	// If the state file exists, it might actually have just been
	// removed by Destroy, and eventual consistency has not caught
	// up yet, so we retry to verify if that is happening.
	var err error
	for a := shortAttempt.Start(); a.Next(); {
		_, err = e.loadState()
		if err != nil {
			break
		}
	}
	if err == nil {
		return fmt.Errorf("environment is already bootstrapped")
	}
	if _, notFound := err.(*environs.NotFoundError); !notFound {
		return fmt.Errorf("cannot query old bootstrap state: %v", err)
	}
	var tools *state.Tools
	if uploadTools {
		tools, err = environs.PutTools(e.Storage(), nil)
		if err != nil {
			return fmt.Errorf("cannot upload tools: %v", err)
		}
	} else {
		flags := environs.HighestVersion | environs.CompatVersion
		v := version.Current
		v.Series = e.Config().DefaultSeries()
		tools, err = environs.FindTools(e, v, flags)
		if err != nil {
			return fmt.Errorf("cannot find tools: %v", err)
		}
	}
	config, err := environs.BootstrapConfig(providerInstance, e.Config(), tools)
	if err != nil {
		return fmt.Errorf("unable to determine inital configuration: %v", err)
	}
	caCert, hasCert := e.Config().CACert()
	if !hasCert {
		return fmt.Errorf("no CA certificate in environment configuration")
	}
	inst, err := e.startInstance(&startInstanceParams{
		machineId: "0",
		info: &state.Info{
			Password: trivial.PasswordHash(password),
			CACert:   caCert,
		},
		apiInfo: &api.Info{
			Password: trivial.PasswordHash(password),
			CACert:   caCert,
		},
		tools:           tools,
		stateServer:     true,
		config:          config,
		stateServerCert: cert,
		stateServerKey:  key,
		withPublicIP:    true,
	})
	if err != nil {
		return fmt.Errorf("cannot start bootstrap instance: %v", err)
	}
	err = e.saveState(&bootstrapState{
		StateInstances: []state.InstanceId{inst.Id()},
	})
	if err != nil {
		// ignore error on StopInstance because the previous error is
		// more important.
		e.StopInstances([]environs.Instance{inst})
		return fmt.Errorf("cannot save state: %v", err)
	}
	// TODO make safe in the case of racing Bootstraps
	// If two Bootstraps are called concurrently, there's
	// no way to use Swift to make sure that only one succeeds.
	// Perhaps consider using SimpleDB for state storage
	// which would enable that possibility.

	return nil
}

func (e *environ) StateInfo() (*state.Info, *api.Info, error) {
	st, err := e.loadState()
	if err != nil {
		return nil, nil, err
	}
	cert, hasCert := e.Config().CACert()
	if !hasCert {
		return nil, nil, fmt.Errorf("no CA certificate in environment configuration")
	}
	var stateAddrs []string
	var apiAddrs []string
	// Wait for the DNS names of any of the instances
	// to become available.
	log.Printf("environs/openstack: waiting for DNS name(s) of state server instances %v", st.StateInstances)
	for a := longAttempt.Start(); len(stateAddrs) == 0 && a.Next(); {
		insts, err := e.Instances(st.StateInstances)
		if err != nil && err != environs.ErrPartialInstances {
			log.Debugf("error getting state instance: %v", err.Error())
			return nil, nil, err
		}
		log.Debugf("started processing instances: %#v", insts)
		for _, inst := range insts {
			if inst == nil {
				continue
			}
			name, err := inst.(*instance).DNSName()
			if err != nil {
				continue
			}
			if name != "" {
				stateAddrs = append(stateAddrs, name+mgoPortSuffix)
				apiAddrs = append(apiAddrs, name+apiPortSuffix)
			}
		}
	}
	if len(stateAddrs) == 0 {
		return nil, nil, fmt.Errorf("timed out waiting for mgo address from %v", st.StateInstances)
	}
	return &state.Info{
			Addrs:  stateAddrs,
			CACert: cert,
		}, &api.Info{
			Addrs:  apiAddrs,
			CACert: cert,
		}, nil
}

func (e *environ) Config() *config.Config {
	return e.ecfg().Config
}

func (e *environ) client(ecfg *environConfig, authModeCfg AuthMode) client.AuthenticatingClient {
	cred := &identity.Credentials{
		User:       ecfg.username(),
		Secrets:    ecfg.password(),
		Region:     ecfg.region(),
		TenantName: ecfg.tenantName(),
		URL:        ecfg.authURL(),
	}
	// authModeCfg has already been validated so we know it's one of the values below.
	var authMode identity.AuthMode
	switch authModeCfg {
	case AuthLegacy:
		authMode = identity.AuthLegacy
	case AuthUserPass:
		authMode = identity.AuthUserPass
	}
	return client.NewClient(cred, authMode, nil)
}

func (e *environ) publicClient(ecfg *environConfig) client.Client {
	return client.NewPublicClient(ecfg.publicBucketURL(), nil)
}

func (e *environ) SetConfig(cfg *config.Config) error {
	ecfg, err := providerInstance.newConfig(cfg)
	if err != nil {
		return err
	}
	// At this point, the authentication method config value has been validated so we extract it's value here
	// to avoid having to validate again each time when creating the OpenStack client.
	var authModeCfg AuthMode
	e.ecfgMutex.Lock()
	defer e.ecfgMutex.Unlock()
	e.name = ecfg.Name()
	authModeCfg = AuthMode(ecfg.authMode())
	e.ecfgUnlocked = ecfg

	novaClient := e.client(ecfg, authModeCfg)
	e.novaUnlocked = nova.New(novaClient)

	// create new storage instances, existing instances continue
	// to reference their existing configuration.
	e.storageUnlocked = &storage{
		containerName: ecfg.controlBucket(),
		// this is possibly just a hack - if the ACL is swift.Private,
		// the machine won't be able to get the tools (401 error)
		containerACL: swift.PublicRead,
		swift:        swift.New(e.client(ecfg, authModeCfg))}
	if ecfg.publicBucket() != "" && ecfg.publicBucketURL() != "" {
		e.publicStorageUnlocked = &storage{
			containerName: ecfg.publicBucket(),
			containerACL:  swift.PublicRead,
			swift:         swift.New(e.publicClient(ecfg))}
	} else {
		e.publicStorageUnlocked = nil
	}

	return nil
}

func (e *environ) StartInstance(machineId string, info *state.Info, apiInfo *api.Info, tools *state.Tools) (environs.Instance, error) {
	return e.startInstance(&startInstanceParams{
		machineId: machineId,
		info:      info,
		apiInfo:   apiInfo,
		tools:     tools,
	})
}

type startInstanceParams struct {
	machineId       string
	info            *state.Info
	apiInfo         *api.Info
	tools           *state.Tools
	stateServer     bool
	config          *config.Config
	stateServerCert []byte
	stateServerKey  []byte

	// withPublicIP, if true causes a floating IP to be
	// assigned to the server after starting
	withPublicIP bool
}

func (e *environ) userData(scfg *startInstanceParams) ([]byte, error) {
	cfg := &cloudinit.MachineConfig{
		StateServer:     scfg.stateServer,
		MongoPort:       mgoPort,
		APIPort:         apiPort,
		StateInfo:       scfg.info,
		APIInfo:         scfg.apiInfo,
		StateServerCert: scfg.stateServerCert,
		StateServerKey:  scfg.stateServerKey,
		// This is a horrible hack, which only works on folsom or
		// later and we'd really like to have a better way
		InstanceIdAccessor: `$(curl http://169.254.169.254/openstack/2012-08-10/meta_data.json|python -c 'import json,sys;print json.loads(sys.stdin.read())["uuid"]')`,
		ProviderType:       "openstack",
		DataDir:            "/var/lib/juju",
		Tools:              scfg.tools,
		MachineId:          scfg.machineId,
		AuthorizedKeys:     e.ecfg().AuthorizedKeys(),
		Config:             scfg.config,
	}
	cloudcfg, err := cloudinit.New(cfg)
	if err != nil {
		return nil, err
	}
	data, err := cloudcfg.Render()
	if err != nil {
		return nil, err
	}
	cdata := trivial.Gzip(data)
	log.Debugf("environs/openstack: openstack user data; %d bytes", len(cdata))
	return cdata, nil
}

// allocatePublicIP tries to find an available floating IP address, or
// allocates a new one, returning it, or an error
func (e *environ) allocatePublicIP() (*nova.FloatingIP, error) {
	fips, err := e.nova().ListFloatingIPs()
	if err != nil {
		return nil, err
	}
	var newfip *nova.FloatingIP
	for _, fip := range fips {
		newfip = &fip
		if fip.InstanceId != nil && *fip.InstanceId != "" {
			// unavailable, skip
			newfip = nil
			continue
		} else {
			// unassigned, we can use it
			return newfip, nil
		}
	}
	if newfip == nil {
		// allocate a new IP and use it
		newfip, err = e.nova().AllocateFloatingIP()
		if err != nil {
			return nil, err
		}
	}
	return newfip, nil
}

// assignPublicIP tries to assign the given floating IP address to the
// specified server, or returns an error.
func (e *environ) assignPublicIP(fip *nova.FloatingIP, serverId string) (err error) {
	if fip == nil {
		return fmt.Errorf("cannot assign a nil public IP to %q", serverId)
	}
	if fip.InstanceId != nil && *fip.InstanceId == serverId {
		// IP already assigned, nothing to do
		return nil
	}
	// At startup nw_info is not yet cached so this may fail
	// temporarily while the server is being built
	for a := longAttempt.Start(); a.Next(); {
		err = e.nova().AddServerFloatingIP(serverId, fip.IP)
		if err == nil {
			return nil
		}
	}
	return err
}

// startInstance is the internal version of StartInstance, used by Bootstrap
// as well as via StartInstance itself.
func (e *environ) startInstance(scfg *startInstanceParams) (environs.Instance, error) {
	var publicIP *nova.FloatingIP
	if scfg.withPublicIP {
		if fip, err := e.allocatePublicIP(); err != nil {
			return nil, fmt.Errorf("cannot allocate a public IP as needed")
		} else {
			publicIP = fip
			log.Printf("environs/openstack: allocated public IP %s", publicIP.IP)
		}
	}
	if scfg.tools == nil {
		var err error
		flags := environs.HighestVersion | environs.CompatVersion
		scfg.tools, err = environs.FindTools(e, version.Current, flags)
		if err != nil {
			return nil, err
		}
	}
	log.Printf("environs/openstack: starting machine %s in %q running tools version %q from %q",
		scfg.machineId, e.name, scfg.tools.Binary, scfg.tools.URL)
	if strings.Contains(scfg.tools.Series, "unknown") || strings.Contains(scfg.tools.Arch, "unknown") {
		return nil, fmt.Errorf("cannot find image for unknown series or architecture")
	}
	spec, err := findInstanceSpec(e, &instanceConstraint{
		series: scfg.tools.Series,
		arch:   scfg.tools.Arch,
		region: e.ecfg().region(),
		flavor: "m1.small",
	})
	if err != nil {
		return nil, fmt.Errorf("cannot find image satisfying constraints: %v", err)
	}

	userData, err := e.userData(scfg)
	if err != nil {
		return nil, fmt.Errorf("cannot make user data: %v", err)
	}
	groups, err := e.setUpGroups(scfg.machineId)
	if err != nil {
		return nil, fmt.Errorf("cannot set up groups: %v", err)
	}
	var groupNames = make([]nova.SecurityGroupName, len(groups))
	for i, g := range groups {
		groupNames[i] = nova.SecurityGroupName{g.Name}
	}

	var server *nova.Entity
	for a := shortAttempt.Start(); a.Next(); {
		server, err = e.nova().RunServer(nova.RunServerOpts{
			Name:               e.machineFullName(scfg.machineId),
			FlavorId:           spec.flavorId,
			ImageId:            spec.imageId,
			UserData:           userData,
			SecurityGroupNames: groupNames,
		})
		if err == nil || !gooseerrors.IsNotFound(err) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("cannot run instance: %v", err)
	}
	detail, err := e.nova().GetServer(server.Id)
	if err != nil {
		return nil, fmt.Errorf("cannot get started instance: %v", err)
	}
	inst := &instance{e, detail, ""}
	log.Printf("environs/openstack: started instance %q", inst.Id())
	if scfg.withPublicIP {
		if err := e.assignPublicIP(publicIP, string(inst.Id())); err != nil {
			if err := e.terminateInstances([]state.InstanceId{inst.Id()}); err != nil {
				// ignore the failure at this stage, just log it
				log.Debugf("environs/openstack: failed to terminate instance %q: %v", inst.Id(), err)
			}
			return nil, fmt.Errorf("cannot assign public address %s to instance %q: %v", publicIP.IP, inst.Id(), err)
		}
		log.Printf("environs/openstack: assigned public IP %s to %q", publicIP.IP, inst.Id())
	}
	return inst, nil
}

func (e *environ) StopInstances(insts []environs.Instance) error {
	ids := make([]state.InstanceId, len(insts))
	for i, inst := range insts {
		instanceValue, ok := inst.(*instance)
		if !ok {
			return errors.New("Incompatible environs.Instance supplied")
		}
		ids[i] = instanceValue.Id()
	}
	log.Debugf("environs/openstack: terminating instances %v", ids)
	return e.terminateInstances(ids)
}

// collectInstances tries to get information on each instance id in ids.
// It fills the slots in the given map for known servers with status
// either ACTIVE or BUILD. Returns a list of missing ids.
func (e *environ) collectInstances(ids []state.InstanceId, out map[state.InstanceId]environs.Instance) []state.InstanceId {
	var err error
	serversById := make(map[string]nova.ServerDetail)
	if len(ids) == 1 {
		// most common case - single instance
		var server *nova.ServerDetail
		server, err = e.nova().GetServer(string(ids[0]))
		if server != nil {
			serversById[server.Id] = *server
		}
	} else {
		var servers []nova.ServerDetail
		servers, err = e.nova().ListServersDetail(e.machinesFilter())
		for _, server := range servers {
			serversById[server.Id] = server
		}
	}
	if err != nil {
		return ids
	}
	var missing []state.InstanceId
	for _, id := range ids {
		if server, found := serversById[string(id)]; found {
			if server.Status == nova.StatusActive || server.Status == nova.StatusBuild {
				out[id] = &instance{e, &server, ""}
			}
			continue
		}
		missing = append(missing, id)
	}
	return missing
}

func (e *environ) Instances(ids []state.InstanceId) ([]environs.Instance, error) {
	missing := ids
	found := make(map[state.InstanceId]environs.Instance)
	// Make a series of requests to cope with eventual consistency.
	// Each request will attempt to add more instances to the requested
	// set.
	for a := shortAttempt.Start(); a.Next(); {
		if missing = e.collectInstances(missing, found); len(missing) == 0 {
			break
		}
	}
	if len(found) == 0 {
		return nil, environs.ErrNoInstances
	}
	insts := make([]environs.Instance, len(ids))
	var err error
	for i, id := range ids {
		if inst := found[id]; inst != nil {
			insts[i] = inst
		} else {
			err = environs.ErrPartialInstances
		}
	}
	return insts, err
}

func (e *environ) AllInstances() (insts []environs.Instance, err error) {
	servers, err := e.nova().ListServersDetail(e.machinesFilter())
	if err != nil {
		return nil, err
	}
	for _, server := range servers {
		if server.Status == nova.StatusActive || server.Status == nova.StatusBuild {
			var s = server
			insts = append(insts, &instance{e, &s, ""})
		}
	}
	return insts, err
}

func (e *environ) Destroy(ensureInsts []environs.Instance) error {
	log.Printf("environs/openstack: destroying environment %q", e.name)
	insts, err := e.AllInstances()
	if err != nil {
		return fmt.Errorf("cannot get instances: %v", err)
	}
	found := make(map[state.InstanceId]bool)
	var ids []state.InstanceId
	for _, inst := range insts {
		ids = append(ids, inst.Id())
		found[inst.Id()] = true
	}

	// Add any instances we've been told about but haven't yet shown
	// up in the instance list.
	for _, inst := range ensureInsts {
		id := state.InstanceId(inst.(*instance).Id())
		if !found[id] {
			ids = append(ids, id)
			found[id] = true
		}
	}
	err = e.terminateInstances(ids)
	if err != nil {
		return err
	}

	// To properly observe e.storageUnlocked we need to get its value while
	// holding e.ecfgMutex. e.Storage() does this for us, then we convert
	// back to the (*storage) to access the private deleteAll() method.
	st := e.Storage().(*storage)
	return st.deleteAll()
}

func (e *environ) AssignmentPolicy() state.AssignmentPolicy {
	panic("AssignmentPolicy not implemented")
}

func (e *environ) globalGroupName() string {
	return fmt.Sprintf("%s-global", e.jujuGroupName())
}

func (e *environ) machineGroupName(machineId string) string {
	return fmt.Sprintf("%s-%s", e.jujuGroupName(), machineId)
}

func (e *environ) jujuGroupName() string {
	return fmt.Sprintf("juju-%s", e.name)
}

func (e *environ) machineFullName(machineId string) string {
	return fmt.Sprintf("juju-%s-%s", e.Name(), state.MachineEntityName(machineId))
}

// machinesFilter returns a nova.Filter matching all machines in the environment.
func (e *environ) machinesFilter() *nova.Filter {
	filter := nova.NewFilter()
	filter.Set(nova.FilterServer, fmt.Sprintf("juju-%s-.*", e.Name()))
	return filter
}

func (e *environ) OpenPorts(ports []state.Port) error {
	panic("OpenPorts not implemented")
}

func (e *environ) ClosePorts(ports []state.Port) error {
	panic("ClosePorts not implemented")
}

func (e *environ) Ports() ([]state.Port, error) {
	panic("Ports not implemented")
}

func (e *environ) Provider() environs.EnvironProvider {
	return &providerInstance
}

// setUpGroups creates the security groups for the new machine, and
// returns them.
//
// Instances are tagged with a group so they can be distinguished from
// other instances that might be running on the same OpenStack account.
// In addition, a specific machine security group is created for each
// machine, so that its firewall rules can be configured per machine.
func (e *environ) setUpGroups(machineId string) ([]nova.SecurityGroup, error) {
	jujuGroup, err := e.ensureGroup(e.jujuGroupName(),
		[]nova.RuleInfo{
			{
				IPProtocol: "tcp",
				FromPort:   22,
				ToPort:     22,
				Cidr:       "0.0.0.0/0",
			},
			{
				IPProtocol: "tcp",
				FromPort:   mgoPort,
				ToPort:     mgoPort,
				Cidr:       "0.0.0.0/0",
			},
			{
				IPProtocol: "tcp",
				FromPort:   1,
				ToPort:     65535,
			},
			{
				IPProtocol: "udp",
				FromPort:   1,
				ToPort:     65535,
			},
			{
				IPProtocol: "icmp",
				FromPort:   -1,
				ToPort:     -1,
			},
		})
	if err != nil {
		return nil, err
	}
	var machineGroup nova.SecurityGroup
	switch e.Config().FirewallMode() {
	case config.FwInstance:
		machineGroup, err = e.ensureGroup(e.machineGroupName(machineId), nil)
	case config.FwGlobal:
		machineGroup, err = e.ensureGroup(e.globalGroupName(), nil)
	}
	if err != nil {
		return nil, err
	}
	return []nova.SecurityGroup{jujuGroup, machineGroup}, nil
}

// zeroGroup holds the zero security group.
var zeroGroup nova.SecurityGroup

// ensureGroup returns the security group with name and perms.
// If a group with name does not exist, one will be created.
// If it exists, its permissions are set to perms.
func (e *environ) ensureGroup(name string, rules []nova.RuleInfo) (nova.SecurityGroup, error) {
	nova := e.nova()
	group, err := nova.CreateSecurityGroup(name, "juju group")
	if err != nil {
		if !gooseerrors.IsDuplicateValue(err) {
			return zeroGroup, err
		} else {
			// We just tried to create a duplicate group, so load the existing group.
			group, err = nova.SecurityGroupByName(name)
			if err != nil {
				return zeroGroup, err
			}
		}
	}
	// The group is created so now add the rules.
	for _, rule := range rules {
		rule.ParentGroupId = group.Id
		_, err := nova.CreateSecurityGroupRule(rule)
		if err != nil && !gooseerrors.IsDuplicateValue(err) {
			return zeroGroup, err
		}
	}
	return *group, nil
}

func (e *environ) terminateInstances(ids []state.InstanceId) error {
	if len(ids) == 0 {
		return nil
	}
	var firstErr error
	nova := e.nova()
	for _, id := range ids {
		err := nova.DeleteServer(string(id))
		if gooseerrors.IsNotFound(err) {
			err = nil
		}
		if err != nil && firstErr == nil {
			log.Debugf("environs/openstack: error terminating instance %q: %v", id, err)
			firstErr = err
		}
	}
	return firstErr
}

// metadataHost holds the address of the instance metadata service.
// It is a variable so that tests can change it to refer to a local
// server when needed.
var metadataHost = "http://169.254.169.254"

// fetchMetadata fetches a single atom of data from the openstack instance metadata service.
// http://docs.amazonwebservices.com/AWSEC2/latest/UserGuide/AESDG-chapter-instancedata.html
// (the same specs is implemented in OpenStack, hence the reference)
func fetchMetadata(name string) (value string, err error) {
	uri := fmt.Sprintf("%s/2011-01-01/meta-data/%s", metadataHost, name)
	for a := shortAttempt.Start(); a.Next(); {
		var resp *http.Response
		resp, err = http.Get(uri)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("bad http response %v", resp.Status)
			continue
		}
		var data []byte
		data, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			continue
		}
		return strings.TrimSpace(string(data)), nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot get %q: %v", uri, err)
	}
	return
}
