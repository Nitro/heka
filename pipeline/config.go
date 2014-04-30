/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#   Mike Trinkala (trink@mozilla.com)
#
# ***** END LICENSE BLOCK *****/

package pipeline

import (
	"code.google.com/p/go-uuid/uuid"
	"fmt"
	"github.com/bbangert/toml"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sync"
	"time"
)

const HEKA_DAEMON = "hekad"

var (
	AvailablePlugins = make(map[string]func() interface{})
	PluginTypeRegex  = regexp.MustCompile("^.*(Decoder|Filter|Input|Output)$")
)

// Adds a plugin to the set of usable Heka plugins that can be referenced from
// a Heka config file.
func RegisterPlugin(name string, factory func() interface{}) {
	AvailablePlugins[name] = factory
}

// Generic plugin configuration type that will be used for plugins that don't
// provide the `HasConfigStruct` interface.
type PluginConfig map[string]toml.Primitive

// API made available to all plugins providing Heka-wide utility functions.
type PluginHelper interface {

	// Returns an `OutputRunner` for an output plugin registered using the
	// specified name, or ok == false if no output by that name is registered.
	Output(name string) (oRunner OutputRunner, ok bool)

	// Returns an `FilterRunner` for a filter plugin registered using the
	// specified name, or ok == false if no filter by that name is registered.
	Filter(name string) (fRunner FilterRunner, ok bool)

	// Returns the currently running Heka instance's unique PipelineConfig
	// object.
	PipelineConfig() *PipelineConfig

	// Instantiates, starts, and returns a DecoderRunner wrapped around a newly
	// created Decoder of the specified name.
	DecoderRunner(base_name, full_name string) (dRunner DecoderRunner, ok bool)

	// Stops and unregisters the provided DecoderRunner.
	StopDecoderRunner(dRunner DecoderRunner) (ok bool)

	// Expects a loop count value from an existing message (or zero if there's
	// no relevant existing message), returns an initialized `PipelinePack`
	// pointer that can be populated w/ message data and inserted into the
	// Heka pipeline. Returns `nil` if the loop count value provided is
	// greater than the maximum allowed by the Heka instance.
	PipelinePack(msgLoopCount uint) *PipelinePack

	// Returns an input plugin of the given name that provides the
	// StatAccumulator interface, or an error value if such a plugin
	// can't be found.
	StatAccumulator(name string) (statAccum StatAccumulator, err error)
}

// Indicates a plug-in has a specific-to-itself config struct that should be
// passed in to its Init method.
type HasConfigStruct interface {
	// Returns a default-value-populated configuration structure into which
	// the plugin's TOML configuration will be deserialized.
	ConfigStruct() interface{}
}

// Indicates a plug-in needs its name before it has access to the runner interface.
type WantsName interface {
	// Passes the toml section name into the plugin at configuration time.
	SetName(name string)
}

// Indicates a plug-in can handle being restart should it exit before
// heka is shut-down.
type Restarting interface {
	// Is called anytime the plug-in returns during the main Run loop to
	// clean up the plug-in state and determine whether the plugin should
	// be restarted or not.
	CleanupForRestart()
}

// Indicates a plug-in can stop without causing a heka shut-down
type Stoppable interface {
	// This function isn't called, the existence of the interface signals
	// the plug-in can safely go away
	IsStoppable()
}

// Master config object encapsulating the entire heka/pipeline configuration.
type PipelineConfig struct {
	// All running InputRunners, by name.
	InputRunners map[string]InputRunner
	// PluginWrappers that can create Input plugin objects.
	inputWrappers map[string]*PluginWrapper
	// PluginWrappers that can create Decoder plugin objects.
	DecoderWrappers map[string]*PluginWrapper
	// All running FilterRunners, by name.
	FilterRunners map[string]FilterRunner
	// PluginWrappers that can create Filter plugin objects.
	filterWrappers map[string]*PluginWrapper
	// All running OutputRunners, by name.
	OutputRunners map[string]OutputRunner
	// PluginWrappers that can create Output plugin objects.
	outputWrappers map[string]*PluginWrapper
	// Heka message router instance.
	router *messageRouter
	// PipelinePack supply for Input plugins.
	inputRecycleChan chan *PipelinePack
	// PipelinePack supply for Filter plugins (separate pool prevents
	// deadlocks).
	injectRecycleChan chan *PipelinePack
	// Stores log messages generated by plugin config errors.
	LogMsgs []string
	// Lock protecting access to the set of running filters so dynamic filters
	// can be safely added and removed while Heka is running.
	filtersLock sync.Mutex
	// Is freed when all FilterRunners have stopped.
	filtersWg sync.WaitGroup
	// Is freed when all DecoderRunners have stopped.
	decodersWg sync.WaitGroup
	// Slice providing access to all running DecoderRunners.
	allDecoders []DecoderRunner
	// Mutex protecting allDecoders.
	allDecodersLock sync.Mutex
	// Name of host on which Heka is running.
	hostname string
	// Heka process id.
	pid int32
	// Lock protecting access to the set of running inputs so they
	// can be safely added while Heka is running.
	inputsLock sync.Mutex
	// Is freed when all Input runners have stopped.
	inputsWg sync.WaitGroup
	// Internal reporting channel
	reportRecycleChan chan *PipelinePack
}

// Creates and initializes a PipelineConfig object. `nil` value for `globals`
// argument means we should use the default global config values.
func NewPipelineConfig(globals *GlobalConfigStruct) (config *PipelineConfig) {
	config = new(PipelineConfig)
	if globals == nil {
		globals = DefaultGlobals()
	}
	// Replace global `Globals` function w/ one that returns our values.
	Globals = func() *GlobalConfigStruct {
		return globals
	}
	config.InputRunners = make(map[string]InputRunner)
	config.inputWrappers = make(map[string]*PluginWrapper)
	config.DecoderWrappers = make(map[string]*PluginWrapper)
	config.FilterRunners = make(map[string]FilterRunner)
	config.filterWrappers = make(map[string]*PluginWrapper)
	config.OutputRunners = make(map[string]OutputRunner)
	config.outputWrappers = make(map[string]*PluginWrapper)
	config.router = NewMessageRouter()
	config.inputRecycleChan = make(chan *PipelinePack, globals.PoolSize)
	config.injectRecycleChan = make(chan *PipelinePack, globals.PoolSize)
	config.LogMsgs = make([]string, 0, 4)
	config.allDecoders = make([]DecoderRunner, 0, 10)
	config.hostname, _ = os.Hostname()
	config.pid = int32(os.Getpid())
	config.reportRecycleChan = make(chan *PipelinePack, 1)

	return config
}

// Callers should pass in the msgLoopCount value from any relevant Message
// objects they are holding. Returns a PipelinePack for injection into Heka
// pipeline, or nil if the msgLoopCount is above the configured maximum.
func (self *PipelineConfig) PipelinePack(msgLoopCount uint) *PipelinePack {
	if msgLoopCount++; msgLoopCount > Globals().MaxMsgLoops {
		return nil
	}
	pack := <-self.injectRecycleChan
	pack.Message.SetTimestamp(time.Now().UnixNano())
	pack.Message.SetUuid(uuid.NewRandom())
	pack.Message.SetHostname(self.hostname)
	pack.Message.SetPid(self.pid)
	pack.RefCount = 1
	pack.MsgLoopCount = msgLoopCount
	return pack
}

// Returns the router.
func (self *PipelineConfig) Router() MessageRouter {
	return self.router
}

// Returns the inputRecycleChannel.
func (self *PipelineConfig) InputRecycleChan() chan *PipelinePack {
	return self.inputRecycleChan
}

// Returns the injectRecycleChannel.
func (self *PipelineConfig) InjectRecycleChan() chan *PipelinePack {
	return self.injectRecycleChan
}

// Returns the hostname.
func (self *PipelineConfig) Hostname() string {
	return self.hostname
}

// Returns OutputRunner registered under the specified name, or nil (and ok ==
// false) if no such name is registered.
func (self *PipelineConfig) Output(name string) (oRunner OutputRunner, ok bool) {
	oRunner, ok = self.OutputRunners[name]
	return
}

// Returns the underlying config object via the Helper interface.
func (self *PipelineConfig) PipelineConfig() *PipelineConfig {
	return self
}

// Instantiates and returns a Decoder of the specified name. Note that any
// time this method is used to fetch an unwrapped Decoder instance, it is up
// to the caller to check for and possibly satisfy the WantsDecoderRunner and
// WantsDecoderRunnerShutdown interfaces.
func (self *PipelineConfig) Decoder(name string) (decoder Decoder, ok bool) {
	var wrapper *PluginWrapper
	if wrapper, ok = self.DecoderWrappers[name]; ok {
		decoder = wrapper.Create().(Decoder)
	}
	return
}

// Instantiates, starts, and returns a DecoderRunner wrapped around a newly
// created Decoder of the specified name.
func (self *PipelineConfig) DecoderRunner(base_name, full_name string) (dRunner DecoderRunner, ok bool) {
	var decoder Decoder
	if decoder, ok = self.Decoder(base_name); ok {
		pluginGlobals := new(PluginGlobals)
		dRunner = NewDecoderRunner(full_name, decoder, pluginGlobals)
		self.allDecodersLock.Lock()
		self.allDecoders = append(self.allDecoders, dRunner)
		self.allDecodersLock.Unlock()
		self.decodersWg.Add(1)
		dRunner.Start(self, &self.decodersWg)
	}
	return
}

// Stops and unregisters the provided DecoderRunner.
func (self *PipelineConfig) StopDecoderRunner(dRunner DecoderRunner) (ok bool) {
	self.allDecodersLock.Lock()
	defer self.allDecodersLock.Unlock()
	for i, r := range self.allDecoders {
		if r == dRunner {
			close(dRunner.InChan())
			self.allDecoders = append(self.allDecoders[:i], self.allDecoders[i+1:]...)
			ok = true
			break
		}
	}
	return
}

// Returns a FilterRunner with the given name, or nil and ok == false if no
// such name is registered.
func (self *PipelineConfig) Filter(name string) (fRunner FilterRunner, ok bool) {
	self.filtersLock.Lock()
	defer self.filtersLock.Unlock()
	fRunner, ok = self.FilterRunners[name]
	return
}

// Returns the specified StatAccumulator input plugin, or an error if it can't
// be found.
func (self *PipelineConfig) StatAccumulator(name string) (statAccum StatAccumulator,
	err error) {

	self.inputsLock.Lock()
	defer self.inputsLock.Unlock()
	iRunner, ok := self.InputRunners[name]
	if !ok {
		err = fmt.Errorf("No Input named '%s", name)
		return
	}
	input := iRunner.Input()
	if statAccum, ok = input.(StatAccumulator); !ok {
		err = fmt.Errorf("Input '%s' is not a StatAccumulator", name)
	}
	return
}

// Starts the provided FilterRunner and adds it to the set of running Filters.
func (self *PipelineConfig) AddFilterRunner(fRunner FilterRunner) error {
	self.filtersLock.Lock()
	defer self.filtersLock.Unlock()
	self.FilterRunners[fRunner.Name()] = fRunner
	self.filtersWg.Add(1)
	if err := fRunner.Start(self, &self.filtersWg); err != nil {
		self.filtersWg.Done()
		return fmt.Errorf("AddFilterRunner '%s' failed to start: %s",
			fRunner.Name(), err)
	} else {
		self.router.AddFilterMatcher() <- fRunner.MatchRunner()
	}
	return nil
}

// Removes the specified FilterRunner from the configuration and the
// MessageRouter which signals the filter to shutdown by closing the input
// channel. Returns true if the filter was removed.
func (self *PipelineConfig) RemoveFilterRunner(name string) bool {
	if Globals().Stopping {
		return false
	}

	self.filtersLock.Lock()
	defer self.filtersLock.Unlock()
	if fRunner, ok := self.FilterRunners[name]; ok {
		self.router.RemoveFilterMatcher() <- fRunner.MatchRunner()
		delete(self.FilterRunners, name)
		return true
	}
	return false
}

// Starts the provided InputRunner and adds it to the set of running Inputs.
func (self *PipelineConfig) AddInputRunner(iRunner InputRunner, wrapper *PluginWrapper) error {
	self.inputsLock.Lock()
	defer self.inputsLock.Unlock()
	if wrapper != nil {
		self.inputWrappers[wrapper.Name] = wrapper
	}
	self.InputRunners[iRunner.Name()] = iRunner
	self.inputsWg.Add(1)
	if err := iRunner.Start(self, &self.inputsWg); err != nil {
		self.inputsWg.Done()
		return fmt.Errorf("AddInputRunner '%s' failed to start: %s", iRunner.Name(), err)
	}
	return nil
}

func (self *PipelineConfig) RemoveInputRunner(iRunner InputRunner) {
	self.inputsLock.Lock()
	defer self.inputsLock.Unlock()
	name := iRunner.Name()
	if _, ok := self.inputWrappers[name]; ok {
		delete(self.inputWrappers, name)
	}
	delete(self.InputRunners, name)
	iRunner.Input().Stop()
}

// Deprecated.
func GetHekaConfigDir(inPath string) string {
	msg := ("`GetHekaConfigDir` is deprecated, please use `PrependBaseDir` or " +
		"`PrependShareDir` instead.")
	Globals().LogMessage("heka", msg)
	return PrependBaseDir(inPath)
}

// Expects either an absolute or relative file path. If absolute, simply
// returns the path unchanged. If relative, prepends
// GlobalConfigStruct.BaseDir.
func PrependBaseDir(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(Globals().BaseDir, path)
}

// Expects either an absolute or relative file path. If absolute, simply
// returns the path unchanged. If relative, prepends
// GlobalConfigStruct.ShareDir.
func PrependShareDir(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(Globals().ShareDir, path)
}

type ConfigFile PluginConfig

// This struct provides a structure for the available retry options for
// a plugin that supports being restarted
type RetryOptions struct {
	// Maximum time in seconds between restart attempts. Defaults to 30s.
	MaxDelay string `toml:"max_delay"`
	// Starting delay in milliseconds between restart attempts. Defaults to
	// 250ms.
	Delay string
	// Maximum jitter added to every retry attempt. Defaults to 500ms.
	MaxJitter string `toml:"max_jitter"`
	// How many times to attempt starting the plugin before failing. Defaults
	// to -1 (retry forever).
	MaxRetries int `toml:"max_retries"`
}

// The TOML spec for plugin configuration options that will be pulled out  by
// Heka itself for runner configuration before the config is passed to the
// Plugin.Init method.
type PluginGlobals struct {
	Typ     string `toml:"type"`
	Ticker  uint   `toml:"ticker_interval"`
	Matcher string `toml:"message_matcher"`
	Signer  string `toml:"message_signer"`
	Retries RetryOptions
}

// Default Decoders configuration.
var defaultDecoderTOML = `
[ProtobufDecoder]
`

// A helper object to support delayed plugin creation.
type PluginWrapper struct {
	Name          string
	ConfigCreator func() interface{}
	PluginCreator func() interface{}
}

// Create a new instance of the plugin and return it. Errors are ignored. Call
// CreateWithError if an error is needed.
func (self *PluginWrapper) Create() (plugin interface{}) {
	plugin, _ = self.CreateWithError()
	return
}

// Create a new instance of the plugin and return it, or nil and appropriate
// error value if this isn't possible.
func (self *PluginWrapper) CreateWithError() (plugin interface{}, err error) {
	plugin = self.PluginCreator()
	if wantsName, ok := plugin.(WantsName); ok {
		wantsName.SetName(self.Name)
	}
	err = plugin.(Plugin).Init(self.ConfigCreator())
	return
}

var unknownOptionRegex = regexp.MustCompile("^Configuration contains key \\[(?P<key>\\S+)\\]")

// If `configable` supports the `HasConfigStruct` interface this will use said
// interface to fetch a config struct object and populate it w/ the values in
// provided `config`. If not, simply returns `config` unchanged.
func LoadConfigStruct(config toml.Primitive, configable interface{}) (
	configStruct interface{}, err error) {

	// On two lines for scoping reasons.
	hasConfigStruct, ok := configable.(HasConfigStruct)
	if !ok {
		// If we don't have a config struct, change it to a PluginConfig
		configStruct = PluginConfig{}
		if err = toml.PrimitiveDecode(config, configStruct); err != nil {
			configStruct = nil
		}
		return
	}

	configStruct = hasConfigStruct.ConfigStruct()

	// Heka defines some common parameters
	// that are defined in the PluginGlobals struct.
	// Use reflection to extract the PluginGlobals fields or TOML tag
	// name if available
	heka_params := make(map[string]interface{})
	pg := PluginGlobals{}
	rt := reflect.ValueOf(pg).Type()
	for i := 0; i < rt.NumField(); i++ {
		sft := rt.Field(i)
		kname := sft.Tag.Get("toml")
		if len(kname) == 0 {
			kname = sft.Name
		}
		heka_params[kname] = true
	}

	if err = toml.PrimitiveDecodeStrict(config, configStruct,
		heka_params); err != nil {
		configStruct = nil
		matches := unknownOptionRegex.FindStringSubmatch(err.Error())
		if len(matches) == 2 {
			// We've got an unrecognized config option.
			err = fmt.Errorf("Unknown config setting: %s", matches[1])
		}
	}
	return
}

// Uses reflection to extract an attribute value from an arbitrary struct type
// that may or may not actually have the attribute, returning a provided
// default if the provided object is not a struct or if the attribute doesn't
// exist.
func getAttr(ob interface{}, attr string, default_ interface{}) (ret interface{}) {
	ret = default_
	obVal := reflect.ValueOf(ob)
	obVal = reflect.Indirect(obVal) // Dereference if it's a pointer.
	if obVal.Kind().String() != "struct" {
		// `FieldByName` will panic if we're not a struct.
		return
	}
	attrVal := obVal.FieldByName(attr)
	if !attrVal.IsValid() {
		return
	}
	return attrVal.Interface()
}

// Used internally to log and record plugin config loading errors.
func (self *PipelineConfig) log(msg string) {
	self.LogMsgs = append(self.LogMsgs, msg)
	log.Println(msg)
}

// loadSection must be passed a plugin name and the config for that plugin. It
// will create a PluginWrapper (i.e. a factory). For decoders we store the
// PluginWrappers and create pools of DecoderRunners for each type, stored in
// our decoder channels. For the other plugin types, we create the plugin,
// configure it, then create the appropriate plugin runner.
func (self *PipelineConfig) loadSection(sectionName string,
	configSection toml.Primitive) (errcnt uint) {
	var ok bool
	var err error
	var pluginGlobals PluginGlobals
	var pluginType string

	wrapper := new(PluginWrapper)
	wrapper.Name = sectionName

	// Setup default retry policy
	pluginGlobals.Retries = RetryOptions{
		MaxDelay:   "30s",
		Delay:      "250ms",
		MaxRetries: -1,
	}

	if err = toml.PrimitiveDecode(configSection, &pluginGlobals); err != nil {
		self.log(fmt.Sprintf("Unable to decode config for plugin: %s, error: %s",
			wrapper.Name, err.Error()))
		errcnt++
		return
	}
	if pluginGlobals.Typ == "" {
		pluginType = sectionName
	} else {
		pluginType = pluginGlobals.Typ
	}

	if wrapper.PluginCreator, ok = AvailablePlugins[pluginType]; !ok {
		self.log(fmt.Sprintf("No such plugin: %s", wrapper.Name))
		errcnt++
		return
	}

	// Create plugin, test config object generation.
	plugin := wrapper.PluginCreator()
	var config interface{}
	if config, err = LoadConfigStruct(configSection, plugin); err != nil {
		self.log(fmt.Sprintf("Can't load config for %s '%s': %s", sectionName,
			wrapper.Name, err))
		errcnt++
		return
	}
	wrapper.ConfigCreator = func() interface{} { return config }
	if wantsName, ok := plugin.(WantsName); ok {
		wantsName.SetName(sectionName)
	}

	// Apply configuration to instantiated plugin.
	if err = plugin.(Plugin).Init(config); err != nil {
		self.log(fmt.Sprintf("Initialization failed for '%s': %s",
			sectionName, err))
		errcnt++
		return
	}

	// Determine the plugin type
	pluginCats := PluginTypeRegex.FindStringSubmatch(pluginType)
	if len(pluginCats) < 2 {
		self.log(fmt.Sprintf("Type doesn't contain valid plugin name: %s", pluginType))
		errcnt++
		return
	}
	pluginCategory := pluginCats[1]

	// Decoders are registered but aren't instantiated until needed by a
	// specific input plugin. We ignore the one that's already been created
	// and just store the wrapper so we can create them when we need them.
	if pluginCategory == "Decoder" {
		self.DecoderWrappers[wrapper.Name] = wrapper
		return
	}

	// If no ticker_interval value was specified in the TOML, we check to see
	// if a default TickerInterval value is specified on the config struct.
	if pluginGlobals.Ticker == 0 {
		tickerVal := getAttr(config, "TickerInterval", uint(0))
		pluginGlobals.Ticker = tickerVal.(uint)
	}

	// For inputs we just store the InputRunner and we're done.
	if pluginCategory == "Input" {
		self.InputRunners[wrapper.Name] = NewInputRunner(wrapper.Name,
			plugin.(Input), &pluginGlobals, false)
		self.inputWrappers[wrapper.Name] = wrapper

		if pluginGlobals.Ticker != 0 {
			tickLength := time.Duration(pluginGlobals.Ticker) * time.Second
			self.InputRunners[wrapper.Name].SetTickLength(tickLength)
		}

		return
	}

	// Filters and outputs have a few more config settings.
	runner := NewFORunner(wrapper.Name, plugin.(Plugin), &pluginGlobals)
	runner.name = wrapper.Name

	if pluginGlobals.Ticker != 0 {
		runner.tickLength = time.Duration(pluginGlobals.Ticker) * time.Second
	}

	// Similarly, if no message_matcher was specified in the TOML we look for
	// a default value on the config struct as a MessageMatcher attribute.
	if pluginGlobals.Matcher == "" {
		matcherVal := getAttr(config, "MessageMatcher", "")
		pluginGlobals.Matcher = matcherVal.(string)
	}

	var matcher *MatchRunner
	if pluginGlobals.Matcher == "" {
		// Filters and outputs must specify a message matcher
		self.log(fmt.Sprintf("'%s' missing message matcher", wrapper.Name))
		errcnt++
		return
	}
	if pluginGlobals.Matcher != "" {
		if matcher, err = NewMatchRunner(pluginGlobals.Matcher,
			pluginGlobals.Signer, runner); err != nil {
			self.log(fmt.Sprintf("Can't create message matcher for '%s': %s",
				wrapper.Name, err))
			errcnt++
			return
		}
		runner.matcher = matcher
	}

	switch pluginCategory {
	case "Filter":
		if matcher != nil {
			self.router.fMatchers = append(self.router.fMatchers, matcher)
		}
		self.FilterRunners[runner.name] = runner
		if _, ok := runner.plugin.(Stoppable); !ok {
			self.filterWrappers[runner.name] = wrapper
		}

	case "Output":
		if matcher != nil {
			self.router.oMatchers = append(self.router.oMatchers, matcher)
		}
		self.OutputRunners[runner.name] = runner
		self.outputWrappers[runner.name] = wrapper
	}

	return
}

// LoadFromConfigFile loads a TOML configuration file and stores the
// result in the value pointed to by config. The maps in the config
// will be initialized as needed.
//
// The PipelineConfig should be already initialized before passed in via
// its Init function.
func (self *PipelineConfig) LoadFromConfigFile(filename string) (err error) {
	var configFile ConfigFile
	if _, err = toml.DecodeFile(filename, &configFile); err != nil {
		return fmt.Errorf("Error decoding config file: %s", err)
	}

	// Load all the plugins
	var errcnt uint
	for name, conf := range configFile {
		if name == HEKA_DAEMON {
			continue
		}
		log.Printf("Loading: [%s]\n", name)
		errcnt += self.loadSection(name, conf)
	}

	// Add JSON/PROTOCOL_BUFFER decoders if none were configured
	var configDefault ConfigFile
	toml.Decode(defaultDecoderTOML, &configDefault)
	dWrappers := self.DecoderWrappers

	if _, ok := dWrappers["ProtobufDecoder"]; !ok {
		log.Println("Loading: [ProtobufDecoder]")
		errcnt += self.loadSection("ProtobufDecoder", configDefault["ProtobufDecoder"])
	}

	if errcnt != 0 {
		return fmt.Errorf("%d errors loading plugins", errcnt)
	}

	return
}
