package sysregistriesv2

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage/pkg/homedir"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// systemRegistriesConfPath is the path to the system-wide registry
// configuration file and is used to add/subtract potential registries for
// obtaining images.  You can override this at build time with
// -ldflags '-X github.com/containers/image/sysregistries.systemRegistriesConfPath=$your_path'
var systemRegistriesConfPath = builtinRegistriesConfPath

// builtinRegistriesConfPath is the path to the registry configuration file.
// DO NOT change this, instead see systemRegistriesConfPath above.
const builtinRegistriesConfPath = "/etc/containers/registries.conf"

// systemRegistriesConfDirPath is the path to the system-wide registry
// configuration directory and is used to add/subtract potential registries for
// obtaining images.  You can override this at build time with
// -ldflags '-X github.com/containers/image/sysregistries.systemRegistriesConfDirecotyPath=$your_path'
var systemRegistriesConfDirPath = builtinRegistriesConfDirPath

// builtinRegistriesConfDirPath is the path to the registry configuration directory.
// DO NOT change this, instead see systemRegistriesConfDirectoryPath above.
const builtinRegistriesConfDirPath = "/etc/containers/registries.conf.d"

// Endpoint describes a remote location of a registry.
type Endpoint struct {
	// The endpoint's remote location.
	Location string `toml:"location,omitempty"`
	// If true, certs verification will be skipped and HTTP (non-TLS)
	// connections will be allowed.
	Insecure bool `toml:"insecure,omitempty"`
}

// userRegistriesFile is the path to the per user registry configuration file.
var userRegistriesFile = filepath.FromSlash(".config/containers/registries.conf")

// userRegistriesDir is the path to the per user registry configuration file.
var userRegistriesDir = filepath.FromSlash(".config/containers/registries.conf.d")

// rewriteReference will substitute the provided reference `prefix` to the
// endpoints `location` from the `ref` and creates a new named reference from it.
// The function errors if the newly created reference is not parsable.
func (e *Endpoint) rewriteReference(ref reference.Named, prefix string) (reference.Named, error) {
	refString := ref.String()
	if !refMatchesPrefix(refString, prefix) {
		return nil, fmt.Errorf("invalid prefix '%v' for reference '%v'", prefix, refString)
	}

	newNamedRef := strings.Replace(refString, prefix, e.Location, 1)
	newParsedRef, err := reference.ParseNamed(newNamedRef)
	if err != nil {
		return nil, errors.Wrapf(err, "error rewriting reference")
	}

	return newParsedRef, nil
}

// Registry represents a registry.
type Registry struct {
	// Prefix is used for matching images, and to translate one namespace to
	// another.  If `Prefix="example.com/bar"`, `location="example.com/foo/bar"`
	// and we pull from "example.com/bar/myimage:latest", the image will
	// effectively be pulled from "example.com/foo/bar/myimage:latest".
	// If no Prefix is specified, it defaults to the specified location.
	Prefix string `toml:"prefix"`
	// A registry is an Endpoint too
	Endpoint
	// The registry's mirrors.
	Mirrors []Endpoint `toml:"mirror,omitempty"`
	// If true, pulling from the registry will be blocked.
	Blocked bool `toml:"blocked,omitempty"`
	// If true, mirrors will only be used for digest pulls. Pulling images by
	// tag can potentially yield different images, depending on which endpoint
	// we pull from.  Forcing digest-pulls for mirrors avoids that issue.
	MirrorByDigestOnly bool `toml:"mirror-by-digest-only,omitempty"`
}

// PullSource consists of an Endpoint and a Reference. Note that the reference is
// rewritten according to the registries prefix and the Endpoint's location.
type PullSource struct {
	Endpoint  Endpoint
	Reference reference.Named
}

// PullSourcesFromReference returns a slice of PullSource's based on the passed
// reference.
func (r *Registry) PullSourcesFromReference(ref reference.Named) ([]PullSource, error) {
	var endpoints []Endpoint

	if r.MirrorByDigestOnly {
		// Only use mirrors when the reference is a digest one.
		if _, isDigested := ref.(reference.Canonical); isDigested {
			endpoints = append(r.Mirrors, r.Endpoint)
		} else {
			endpoints = []Endpoint{r.Endpoint}
		}
	} else {
		endpoints = append(r.Mirrors, r.Endpoint)
	}

	sources := []PullSource{}
	for _, ep := range endpoints {
		rewritten, err := ep.rewriteReference(ref, r.Prefix)
		if err != nil {
			return nil, err
		}
		sources = append(sources, PullSource{Endpoint: ep, Reference: rewritten})
	}

	return sources, nil
}

// V1TOMLregistries is for backwards compatibility to sysregistries v1
type V1TOMLregistries struct {
	Registries []string `toml:"registries"`
}

// V1TOMLConfig is for backwards compatibility to sysregistries v1
type V1TOMLConfig struct {
	Search   V1TOMLregistries `toml:"search"`
	Insecure V1TOMLregistries `toml:"insecure"`
	Block    V1TOMLregistries `toml:"block"`
}

// V1RegistriesConf is the sysregistries v1 configuration format.
type V1RegistriesConf struct {
	V1TOMLConfig `toml:"registries"`
}

// Nonempty returns true if config contains at least one configuration entry.
func (config *V1RegistriesConf) Nonempty() bool {
	return (len(config.V1TOMLConfig.Search.Registries) != 0 ||
		len(config.V1TOMLConfig.Insecure.Registries) != 0 ||
		len(config.V1TOMLConfig.Block.Registries) != 0)
}

// V2RegistriesConf is the sysregistries v2 configuration format.
type V2RegistriesConf struct {
	Registries []Registry `toml:"registry"`
	// An array of host[:port] (not prefix!) entries to use for resolving unqualified image references
	UnqualifiedSearchRegistries []string `toml:"unqualified-search-registries"`

	// ShortNameMode defines how short-name resolution should be handled by
	// _consumers_ of this package.  Depending on the mode, the user should
	// be prompted with a choice of using one of the unqualified-search
	// registries when referring to a short name.
	//
	// Valid modes are: * "prompt": prompt if stdout is a TTY, otherwise
	// use all unqualified-search registries * "enforcing": always prompt
	// and error if stdout is not a TTY * "disabled": do not prompt and
	// potentially use all unqualified-search registries
	ShortNameMode string `toml:"short-name-mode"`

	// TODO: separate upper format from internal data below:
	// https://github.com/containers/image/pull/1060#discussion_r503386541

	// shortNameMode is stored _once_ when loading the config.
	shortNameMode types.ShortNameMode

	shortNameAliasConf
}

// Nonempty returns true if config contains at least one configuration entry.
func (config *V2RegistriesConf) Nonempty() bool {
	return (len(config.Registries) != 0 ||
		len(config.UnqualifiedSearchRegistries) != 0)
}

// parsedConfig is the result of parsing, and possibly merging, configuration files;
// it is the boundary between the process of reading+ingesting the files, and
// later interpreting the configuraiton based on caller’s requests.
type parsedConfig struct {
	// partialV2 must continue to exist to maintain the return value of TryUpdatingCache
	// for compatibility with existing callers.
	// We store the authoritative Registries and UnqualifiedSearchRegistries values there as well.
	partialV2 V2RegistriesConf
	// Absolute path to the configuration file that set the UnqualifiedSearchRegistries.
	unqualifiedSearchRegistriesOrigin string
	aliasCache                        *shortNameAliasCache
}

// InvalidRegistries represents an invalid registry configurations.  An example
// is when "registry.com" is defined multiple times in the configuration but
// with conflicting security settings.
type InvalidRegistries struct {
	s string
}

// Error returns the error string.
func (e *InvalidRegistries) Error() string {
	return e.s
}

// parseLocation parses the input string, performs some sanity checks and returns
// the sanitized input string.  An error is returned if the input string is
// empty or if contains an "http{s,}://" prefix.
func parseLocation(input string) (string, error) {
	trimmed := strings.TrimRight(input, "/")

	if trimmed == "" {
		return "", &InvalidRegistries{s: "invalid location: cannot be empty"}
	}

	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		msg := fmt.Sprintf("invalid location '%s': URI schemes are not supported", input)
		return "", &InvalidRegistries{s: msg}
	}

	return trimmed, nil
}

// ConvertToV2 returns a v2 config corresponding to a v1 one.
func (config *V1RegistriesConf) ConvertToV2() (*V2RegistriesConf, error) {
	regMap := make(map[string]*Registry)
	// The order of the registries is not really important, but make it deterministic (the same for the same config file)
	// to minimize behavior inconsistency and not contribute to difficult-to-reproduce situations.
	registryOrder := []string{}

	getRegistry := func(location string) (*Registry, error) { // Note: _pointer_ to a long-lived object
		var err error
		location, err = parseLocation(location)
		if err != nil {
			return nil, err
		}
		reg, exists := regMap[location]
		if !exists {
			reg = &Registry{
				Endpoint: Endpoint{Location: location},
				Mirrors:  []Endpoint{},
				Prefix:   location,
			}
			regMap[location] = reg
			registryOrder = append(registryOrder, location)
		}
		return reg, nil
	}

	for _, blocked := range config.V1TOMLConfig.Block.Registries {
		reg, err := getRegistry(blocked)
		if err != nil {
			return nil, err
		}
		reg.Blocked = true
	}
	for _, insecure := range config.V1TOMLConfig.Insecure.Registries {
		reg, err := getRegistry(insecure)
		if err != nil {
			return nil, err
		}
		reg.Insecure = true
	}

	res := &V2RegistriesConf{
		UnqualifiedSearchRegistries: config.V1TOMLConfig.Search.Registries,
	}
	for _, location := range registryOrder {
		reg := regMap[location]
		res.Registries = append(res.Registries, *reg)
	}
	return res, nil
}

// anchoredDomainRegexp is an internal implementation detail of postProcess, defining the valid values of elements of UnqualifiedSearchRegistries.
var anchoredDomainRegexp = regexp.MustCompile("^" + reference.DomainRegexp.String() + "$")

// postProcess checks the consistency of all the configuration, looks for conflicts,
// and normalizes the configuration (e.g., sets the Prefix to Location if not set).
func (config *V2RegistriesConf) postProcessRegistries() error {
	regMap := make(map[string][]*Registry)

	for i := range config.Registries {
		reg := &config.Registries[i]
		// make sure Location and Prefix are valid
		var err error
		reg.Location, err = parseLocation(reg.Location)
		if err != nil {
			return err
		}

		if reg.Prefix == "" {
			reg.Prefix = reg.Location
		} else {
			reg.Prefix, err = parseLocation(reg.Prefix)
			if err != nil {
				return err
			}
		}

		// make sure mirrors are valid
		for _, mir := range reg.Mirrors {
			mir.Location, err = parseLocation(mir.Location)
			if err != nil {
				return err
			}
		}
		regMap[reg.Location] = append(regMap[reg.Location], reg)
	}

	// Given a registry can be mentioned multiple times (e.g., to have
	// multiple prefixes backed by different mirrors), we need to make sure
	// there are no conflicts among them.
	//
	// Note: we need to iterate over the registries array to ensure a
	// deterministic behavior which is not guaranteed by maps.
	for _, reg := range config.Registries {
		others, ok := regMap[reg.Location]
		if !ok {
			return fmt.Errorf("Internal error in V2RegistriesConf.PostProcess: entry in regMap is missing")
		}
		for _, other := range others {
			if reg.Insecure != other.Insecure {
				msg := fmt.Sprintf("registry '%s' is defined multiple times with conflicting 'insecure' setting", reg.Location)
				return &InvalidRegistries{s: msg}
			}

			if reg.Blocked != other.Blocked {
				msg := fmt.Sprintf("registry '%s' is defined multiple times with conflicting 'blocked' setting", reg.Location)
				return &InvalidRegistries{s: msg}
			}
		}
	}

	for i := range config.UnqualifiedSearchRegistries {
		registry, err := parseLocation(config.UnqualifiedSearchRegistries[i])
		if err != nil {
			return err
		}
		if !anchoredDomainRegexp.MatchString(registry) {
			return &InvalidRegistries{fmt.Sprintf("Invalid unqualified-search-registries entry %#v", registry)}
		}
		config.UnqualifiedSearchRegistries[i] = registry
	}

	// Registries are ordered and the first longest prefix always wins,
	// rendering later items with the same prefix non-existent. We cannot error
	// out anymore as this might break existing users, so let's just ignore them
	// to guarantee that the same prefix exists only once.
	//
	// As a side effect of parsedConfig.updateWithConfigurationFrom, the Registries slice
	// is always sorted. To be consistent in situations where it is not called (no drop-ins),
	// sort it here as well.
	prefixes := []string{}
	uniqueRegistries := make(map[string]Registry)
	for i := range config.Registries {
		// TODO: should we warn if we see the same prefix being used multiple times?
		prefix := config.Registries[i].Prefix
		if _, exists := uniqueRegistries[prefix]; !exists {
			uniqueRegistries[prefix] = config.Registries[i]
			prefixes = append(prefixes, prefix)
		}
	}
	sort.Strings(prefixes)
	config.Registries = []Registry{}
	for _, prefix := range prefixes {
		config.Registries = append(config.Registries, uniqueRegistries[prefix])
	}

	return nil
}

// ConfigPath returns the path to the system-wide registry configuration file.
// Deprecated: This API implies configuration is read from files, and that there is only one.
// Please use ConfigurationSourceDescription to obtain a string usable for error messages.
func ConfigPath(ctx *types.SystemContext) string {
	return newConfigWrapper(ctx).configPath
}

// ConfigDirPath returns the path to the directory for drop-in
// registry configuration files.
// Deprecated: This API implies configuration is read from directories, and that there is only one.
// Please use ConfigurationSourceDescription to obtain a string usable for error messages.
func ConfigDirPath(ctx *types.SystemContext) string {
	configWrapper := newConfigWrapper(ctx)
	if configWrapper.userConfigDirPath != "" {
		return configWrapper.userConfigDirPath
	}
	return configWrapper.configDirPath
}

// configWrapper is used to store the paths from ConfigPath and ConfigDirPath
// and acts as a key to the internal cache.
type configWrapper struct {
	// path to the registries.conf file
	configPath string
	// path to system-wide registries.conf.d directory, or "" if not used
	configDirPath string
	// path to user specified registries.conf.d directory, or "" if not used
	userConfigDirPath string
}

// newConfigWrapper returns a configWrapper for the specified SystemContext.
func newConfigWrapper(ctx *types.SystemContext) configWrapper {
	var wrapper configWrapper
	userRegistriesFilePath := filepath.Join(homedir.Get(), userRegistriesFile)
	userRegistriesDirPath := filepath.Join(homedir.Get(), userRegistriesDir)

	// decide configPath using per-user path or system file
	if ctx != nil && ctx.SystemRegistriesConfPath != "" {
		wrapper.configPath = ctx.SystemRegistriesConfPath
	} else if _, err := os.Stat(userRegistriesFilePath); err == nil {
		// per-user registries.conf exists, not reading system dir
		// return config dirs from ctx or per-user one
		wrapper.configPath = userRegistriesFilePath
		if ctx != nil && ctx.SystemRegistriesConfDirPath != "" {
			wrapper.configDirPath = ctx.SystemRegistriesConfDirPath
		} else {
			wrapper.userConfigDirPath = userRegistriesDirPath
		}

		return wrapper
	} else if ctx != nil && ctx.RootForImplicitAbsolutePaths != "" {
		wrapper.configPath = filepath.Join(ctx.RootForImplicitAbsolutePaths, systemRegistriesConfPath)
	} else {
		wrapper.configPath = systemRegistriesConfPath
	}

	// potentially use both system and per-user dirs if not using per-user config file
	if ctx != nil && ctx.SystemRegistriesConfDirPath != "" {
		// dir explicitly chosen: use only that one
		wrapper.configDirPath = ctx.SystemRegistriesConfDirPath
	} else if ctx != nil && ctx.RootForImplicitAbsolutePaths != "" {
		wrapper.configDirPath = filepath.Join(ctx.RootForImplicitAbsolutePaths, systemRegistriesConfDirPath)
		wrapper.userConfigDirPath = userRegistriesDirPath
	} else {
		wrapper.configDirPath = systemRegistriesConfDirPath
		wrapper.userConfigDirPath = userRegistriesDirPath
	}

	return wrapper
}

// ConfigurationSourceDescription returns a string containres paths of registries.conf and registries.conf.d
func ConfigurationSourceDescription(ctx *types.SystemContext) string {
	wrapper := newConfigWrapper(ctx)
	configSources := []string{wrapper.configPath}
	if wrapper.configDirPath != "" {
		configSources = append(configSources, wrapper.configDirPath)
	}
	if wrapper.userConfigDirPath != "" {
		configSources = append(configSources, wrapper.userConfigDirPath)
	}
	return strings.Join(configSources, ", ")
}

// configMutex is used to synchronize concurrent accesses to configCache.
var configMutex = sync.Mutex{}

// configCache caches already loaded configs with config paths as keys and is
// used to avoid redundantly parsing configs. Concurrent accesses to the cache
// are synchronized via configMutex.
var configCache = make(map[configWrapper]*parsedConfig)

// InvalidateCache invalidates the registry cache.  This function is meant to be
// used for long-running processes that need to reload potential changes made to
// the cached registry config files.
func InvalidateCache() {
	configMutex.Lock()
	defer configMutex.Unlock()
	configCache = make(map[configWrapper]*parsedConfig)
}

// getConfig returns the config object corresponding to ctx, loading it if it is not yet cached.
func getConfig(ctx *types.SystemContext) (*parsedConfig, error) {
	wrapper := newConfigWrapper(ctx)
	configMutex.Lock()
	if config, inCache := configCache[wrapper]; inCache {
		configMutex.Unlock()
		return config, nil
	}
	configMutex.Unlock()

	return tryUpdatingCache(ctx, wrapper)
}

// dropInConfigs returns a slice of drop-in-configs from the registries.conf.d
// directory.
func dropInConfigs(wrapper configWrapper) ([]string, error) {
	var (
		configs  []string
		dirPaths []string
	)
	if wrapper.configDirPath != "" {
		dirPaths = append(dirPaths, wrapper.configDirPath)
	}
	if wrapper.userConfigDirPath != "" {
		dirPaths = append(dirPaths, wrapper.userConfigDirPath)
	}
	for _, dirPath := range dirPaths {
		err := filepath.Walk(dirPath,
			// WalkFunc to read additional configs
			func(path string, info os.FileInfo, err error) error {
				switch {
				case err != nil:
					// return error (could be a permission problem)
					return err
				case info == nil:
					// this should only happen when err != nil but let's be sure
					return nil
				case info.IsDir():
					if path != dirPath {
						// make sure to not recurse into sub-directories
						return filepath.SkipDir
					}
					// ignore directories
					return nil
				default:
					// only add *.conf files
					if strings.HasSuffix(path, ".conf") {
						configs = append(configs, path)
					}
					return nil
				}
			},
		)

		if err != nil && !os.IsNotExist(err) {
			// Ignore IsNotExist errors: most systems won't have a registries.conf.d
			// directory.
			return nil, errors.Wrapf(err, "error reading registries.conf.d")
		}
	}

	return configs, nil
}

// TryUpdatingCache loads the configuration from the provided `SystemContext`
// without using the internal cache. On success, the loaded configuration will
// be added into the internal registry cache.
// It returns the resulting configuration; this is DEPRECATED and may not correctly
// reflect any future data handled by this package.
func TryUpdatingCache(ctx *types.SystemContext) (*V2RegistriesConf, error) {
	config, err := tryUpdatingCache(ctx, newConfigWrapper(ctx))
	if err != nil {
		return nil, err
	}
	return &config.partialV2, err
}

// tryUpdatingCache implements TryUpdatingCache with an additional configWrapper
// argument to avoid redundantly calculating the config paths.
func tryUpdatingCache(ctx *types.SystemContext, wrapper configWrapper) (*parsedConfig, error) {
	configMutex.Lock()
	defer configMutex.Unlock()

	// load the config
	config := &parsedConfig{}
	if err := config.loadConfig(wrapper.configPath, false); err != nil {
		// Continue with an empty []Registry if we use the default config, which
		// implies that the config path of the SystemContext isn't set.
		//
		// Note: if ctx.SystemRegistriesConfPath points to the default config,
		// we will still return an error.
		if os.IsNotExist(err) && (ctx == nil || ctx.SystemRegistriesConfPath == "") {
			config = &parsedConfig{}
			config.partialV2 = V2RegistriesConf{Registries: []Registry{}}
		} else {
			return nil, errors.Wrapf(err, "error loading registries configuration %q", wrapper.configPath)
		}
	}

	// Load the configs from the conf directory path.
	dinConfigs, err := dropInConfigs(wrapper)
	if err != nil {
		return nil, err
	}
	for _, path := range dinConfigs {
		// Enforce v2 format for drop-in-configs.
		if err := config.loadConfig(path, true); err != nil {
			return nil, errors.Wrapf(err, "error loading drop-in registries configuration %q", path)
		}
	}

	// populate the cache
	configCache[wrapper] = config
	return config, nil
}

// GetRegistries loads and returns the registries specified in the config.
// Note the parsed content of registry config files is cached.  For reloading,
// use `InvalidateCache` and re-call `GetRegistries`.
func GetRegistries(ctx *types.SystemContext) ([]Registry, error) {
	config, err := getConfig(ctx)
	if err != nil {
		return nil, err
	}
	return config.partialV2.Registries, nil
}

// UnqualifiedSearchRegistries returns a list of host[:port] entries to try
// for unqualified image search, in the returned order)
func UnqualifiedSearchRegistries(ctx *types.SystemContext) ([]string, error) {
	registries, _, err := UnqualifiedSearchRegistriesWithOrigin(ctx)
	return registries, err
}

// UnqualifiedSearchRegistriesWithOrigin returns a list of host[:port] entries
// to try for unqualified image search, in the returned order.  It also returns
// a human-readable description of where these entries are specified (e.g., a
// registries.conf file).
func UnqualifiedSearchRegistriesWithOrigin(ctx *types.SystemContext) ([]string, string, error) {
	config, err := getConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	return config.partialV2.UnqualifiedSearchRegistries, config.unqualifiedSearchRegistriesOrigin, nil
}

// parseShortNameMode translates the string into well-typed
// types.ShortNameMode.
func parseShortNameMode(mode string) (types.ShortNameMode, error) {
	switch mode {
	case "disabled":
		return types.ShortNameModeDisabled, nil
	case "enforcing":
		return types.ShortNameModeEnforcing, nil
	case "permissive":
		return types.ShortNameModePermissive, nil
	default:
		return types.ShortNameModeInvalid, errors.Errorf("invalid short-name mode: %q", mode)
	}
}

// GetShortNameMode returns the configured types.ShortNameMode.
func GetShortNameMode(ctx *types.SystemContext) (types.ShortNameMode, error) {
	config, err := getConfig(ctx)
	if err != nil {
		return -1, err
	}
	return config.partialV2.shortNameMode, err
}

// refMatchesPrefix returns true iff ref,
// which is a registry, repository namespace, repository or image reference (as formatted by
// reference.Domain(), reference.Named.Name() or reference.Reference.String()
// — note that this requires the name to start with an explicit hostname!),
// matches a Registry.Prefix value.
// (This is split from the caller primarily to make testing easier.)
func refMatchesPrefix(ref, prefix string) bool {
	switch {
	case len(ref) < len(prefix):
		return false
	case len(ref) == len(prefix):
		return ref == prefix
	case len(ref) > len(prefix):
		if !strings.HasPrefix(ref, prefix) {
			return false
		}
		c := ref[len(prefix)]
		// This allows "example.com:5000" to match "example.com",
		// which is unintended; that will get fixed eventually, DON'T RELY
		// ON THE CURRENT BEHAVIOR.
		return c == ':' || c == '/' || c == '@'
	default:
		panic("Internal error: impossible comparison outcome")
	}
}

// FindRegistry returns the Registry with the longest prefix for ref,
// which is a registry, repository namespace repository or image reference (as formatted by
// reference.Domain(), reference.Named.Name() or reference.Reference.String()
// — note that this requires the name to start with an explicit hostname!).
// If no Registry prefixes the image, nil is returned.
func FindRegistry(ctx *types.SystemContext, ref string) (*Registry, error) {
	config, err := getConfig(ctx)
	if err != nil {
		return nil, err
	}

	reg := Registry{}
	prefixLen := 0
	for _, r := range config.partialV2.Registries {
		if refMatchesPrefix(ref, r.Prefix) {
			length := len(r.Prefix)
			if length > prefixLen {
				reg = r
				prefixLen = length
			}
		}
	}
	if prefixLen != 0 {
		return &reg, nil
	}
	return nil, nil
}

// loadConfigFile loads and unmarshals a single config file, updating *v2 and also returning
// some parsed data in the return value.
// Use forceV2 if the config must in the v2 format.
func loadConfigFile(v2 *V2RegistriesConf, path string, forceV2 bool) (*parsedConfig, error) {
	logrus.Debugf("Loading registries configuration %q", path)

	// tomlConfig allows us to unmarshal either V1 or V2 simultaneously.
	type tomlConfig struct {
		V2RegistriesConf
		V1RegistriesConf // for backwards compatibility with sysregistries v1
	}

	// Load the tomlConfig. Note that `DecodeFile` will overwrite set fields.
	combinedTOML := tomlConfig{
		V2RegistriesConf: *v2,
	}
	_, err := toml.DecodeFile(path, &combinedTOML)
	if err != nil {
		return nil, err
	}

	if combinedTOML.V1RegistriesConf.Nonempty() {
		// Enforce the v2 format if requested.
		if forceV2 {
			return nil, &InvalidRegistries{s: "registry must be in v2 format but is in v1"}
		}

		// Convert a v1 config into a v2 config.
		if combinedTOML.V2RegistriesConf.Nonempty() {
			return nil, &InvalidRegistries{s: "mixing sysregistry v1/v2 is not supported"}
		}
		converted, err := combinedTOML.V1RegistriesConf.ConvertToV2()
		if err != nil {
			return nil, err
		}
		combinedTOML.V1RegistriesConf = V1RegistriesConf{}
		combinedTOML.V2RegistriesConf = *converted
	}

	res := parsedConfig{partialV2: combinedTOML.V2RegistriesConf}

	// Post process registries, set the correct prefixes, sanity checks, etc.
	if err := res.partialV2.postProcessRegistries(); err != nil {
		return nil, err
	}

	res.unqualifiedSearchRegistriesOrigin = path

	// Parse and validate short-name aliases.
	cache, err := newShortNameAliasCache(path, &res.partialV2.shortNameAliasConf)
	if err != nil {
		return nil, errors.Wrap(err, "error validating short-name aliases")
	}
	res.aliasCache = cache
	// Clear conf.partialV2.shortNameAliasConf to make it available for garbage collection and
	// reduce memory consumption.  We're consulting aliasCache for lookups.
	res.partialV2.shortNameAliasConf = shortNameAliasConf{}

	*v2 = res.partialV2
	return &res, nil
}

// loadConfig loads and unmarshals the configuration at the specified path.
// Use forceV2 if the config must in the v2 format.
//
// Note that specified fields in path will replace already set fields in the
// parsedConfig.  Only the [[registry]] tables are merged by prefix.
func (c *parsedConfig) loadConfig(path string, forceV2 bool) error {
	// Save the registries before decoding the file where they could be lost.
	// We merge them later again.
	registryMap := make(map[string]Registry)
	for i := range c.partialV2.Registries {
		registryMap[c.partialV2.Registries[i].Prefix] = c.partialV2.Registries[i]
	}

	// Store the current USRs so we can determine _after_ loading if they
	// changed.
	prevUSRs := c.partialV2.UnqualifiedSearchRegistries
	c.partialV2.UnqualifiedSearchRegistries = nil

	// Load the new config file. Note that loadConfigFile will overwrite set fields.
	c.partialV2.Registries = nil // important to clear the memory to prevent us from overlapping fields
	c.partialV2.Aliases = nil
	updates, err := loadConfigFile(&c.partialV2, path, forceV2)
	if err != nil {
		return err
	}

	// Now check if the newly loaded config set the USRs.
	if c.partialV2.UnqualifiedSearchRegistries != nil {
		// USRs set -> record it as the new origin.
		c.unqualifiedSearchRegistriesOrigin = updates.unqualifiedSearchRegistriesOrigin
	} else {
		// USRs not set -> restore the previous USRs
		c.partialV2.UnqualifiedSearchRegistries = prevUSRs
	}

	// Merge the freshly loaded registries.
	for i := range c.partialV2.Registries {
		registryMap[c.partialV2.Registries[i].Prefix] = c.partialV2.Registries[i]
	}

	// Go maps have a non-deterministic order when iterating the keys, so
	// we dump them in a slice and sort it to enforce some order in
	// Registries slice.  Some consumers of c/image (e.g., CRI-O) log the
	// the configuration where a non-deterministic order could easily cause
	// confusion.
	prefixes := []string{}
	for prefix := range registryMap {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)

	c.partialV2.Registries = []Registry{}
	for _, prefix := range prefixes {
		c.partialV2.Registries = append(c.partialV2.Registries, registryMap[prefix])
	}

	// Merge the alias maps.  New configs override previous entries.
	if c.aliasCache == nil {
		c.aliasCache = &shortNameAliasCache{
			namedAliases: make(map[string]alias),
		}
	}
	c.aliasCache.updateWithConfigurationFrom(updates.aliasCache)

	// If set, parse & store the specified short-name mode.
	if len(c.partialV2.ShortNameMode) > 0 {
		mode, err := parseShortNameMode(c.partialV2.ShortNameMode)
		if err != nil {
			return err
		}
		c.partialV2.shortNameMode = mode
	} else {
		c.partialV2.shortNameMode = defaultShortNameMode
	}

	return nil
}
