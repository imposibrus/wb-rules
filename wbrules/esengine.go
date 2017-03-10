package wbrules

import (
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/DisposaBoy/JsonConfigReader"
	"github.com/boltdb/bolt"
	duktape "github.com/contactless/go-duktape"
	wbgo "github.com/contactless/wbgo"
	"github.com/stretchr/objx"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type itemType int

const (
	LIB_FILE            = "lib.js"
	LIB_SYS_PATH        = "/usr/share/wb-rules-system/scripts"
	LIB_REL_PATH_1      = "scripts"
	LIB_REL_PATH_2      = "../scripts"
	MIN_INTERVAL_MS     = 1
	PERSISTENT_DB_CHMOD = 0640
	SOURCE_ITEM_DEVICE  = itemType(iota)
	SOURCE_ITEM_RULE

	MODULE_FILENAME_PROP = "filename"
	MODULE_STORAGE_PROP  = "storage"

	GLOBAL_OBJ_PROTO_NAME = "__wbGlobalPrototype"
	MODULE_OBJ_PROTO_NAME = "__wbModulePrototype"

	VDEV_OBJ_PROP_DEVID = "__deviceId"
	VDEV_OBJ_PROTO_NAME = "__wbVdevPrototype"

	THREAD_STORAGE_OBJ_NAME       = "_esThreads"
	MODULES_USER_STORAGE_OBJ_NAME = "_esModules"
	GLOBAL_INIT_ENV_FUNC_NAME     = "__esInitEnv"
)

var noSuchPropError = errors.New("no such property")
var wrongPropTypeError = errors.New("wrong property type")

var noLibJs = errors.New("unable to locate lib.js")
var searchDirs = []string{LIB_SYS_PATH}

// cache for quicker filename hashing
var filenameMd5s = make(map[string]string)

type sourceMap map[string]*LocFileEntry

type ESEngineOptions struct {
	*RuleEngineOptions
	PersistentDBFile     string
	PersistentDBFileMode os.FileMode
	ModulesDirs          []string
}

func NewESEngineOptions() *ESEngineOptions {
	return &ESEngineOptions{
		RuleEngineOptions:    NewRuleEngineOptions(),
		PersistentDBFileMode: PERSISTENT_DB_CHMOD,
	}
}

func (o *ESEngineOptions) SetPersistentDBFile(file string) {
	o.PersistentDBFile = file
}

func (o *ESEngineOptions) SetPersistentDBFileMode(mode os.FileMode) {
	o.PersistentDBFileMode = mode
}

func (o *ESEngineOptions) SetModulesDirs(dirs []string) {
	o.ModulesDirs = dirs
}

type TimerSet struct {
	sync.Mutex
	timers map[TimerId]bool
}

func newTimerSet() *TimerSet {
	return &TimerSet{
		timers: make(map[TimerId]bool),
	}
}

type ESEngine struct {
	*RuleEngine
	ctxFactory        *ESContextFactory     // ESContext factory
	globalCtx         *ESContext            // global context - prototype for local contexts in threads
	localCtxs         map[string]*ESContext // local scripts' contexts, mapped from script paths
	ctxTimers         map[*ESContext]*TimerSet
	sourceRoot        string
	sources           sourceMap
	currentSource     *LocFileEntry
	sourcesMtx        sync.Mutex
	tracker           *wbgo.ContentTracker
	persistentDBCache map[string]string
	persistentDB      *bolt.DB
	modulesDirs       []string
}

func init() {
	if wd, err := os.Getwd(); err == nil {
		searchDirs = []string{
			LIB_SYS_PATH,
			filepath.Join(wd, LIB_REL_PATH_1),
			filepath.Join(wd, LIB_REL_PATH_2),
		}
	}
}

func NewESEngine(model *CellModel, mqttClient wbgo.MQTTClient, options *ESEngineOptions) (engine *ESEngine) {
	if options == nil {
		panic("no options given to NewESEngine")
	}

	engine = &ESEngine{
		RuleEngine:        NewRuleEngine(model, mqttClient, options.RuleEngineOptions),
		ctxFactory:        newESContextFactory(),
		localCtxs:         make(map[string]*ESContext),
		ctxTimers:         make(map[*ESContext]*TimerSet),
		sources:           make(sourceMap),
		tracker:           wbgo.NewContentTracker(),
		persistentDBCache: make(map[string]string),
		persistentDB:      nil,
		modulesDirs:       options.ModulesDirs,
	}
	engine.globalCtx = engine.ctxFactory.newESContext(model.CallSync, "")

	if options.PersistentDBFile != "" {
		if err := engine.SetPersistentDBMode(options.PersistentDBFile,
			options.PersistentDBFileMode); err != nil {
			panic("error opening persistent DB file: " + err.Error())
		}
	}

	engine.globalCtx.SetCallbackErrorHandler(engine.CallbackErrorHandler)

	// init modSearch for global
	engine.exportModSearch(engine.globalCtx)

	// init __wbModulePrototype
	engine.initModulePrototype(engine.globalCtx)

	// init virtual device prototype
	engine.initVdevPrototype(engine.globalCtx)

	// init threads storage
	engine.initGlobalThreadList(engine.globalCtx)

	// init modules storage
	engine.initModulesStorage(engine.globalCtx)

	engine.globalCtx.PushGlobalObject()

	engine.globalCtx.DefineFunctions(map[string]func(*ESContext) int{
		"format":               engine.esFormat,
		"log":                  engine.makeLogFunc(ENGINE_LOG_INFO),
		"debug":                engine.makeLogFunc(ENGINE_LOG_DEBUG),
		"publish":              engine.esPublish,
		"_wbDevObject":         engine.esWbDevObject,
		"_wbCellObject":        engine.esWbCellObject,
		"_wbStartTimer":        engine.esWbStartTimer,
		"_wbStopTimer":         engine.esWbStopTimer,
		"_wbCheckCurrentTimer": engine.esWbCheckCurrentTimer,
		"_wbSpawn":             engine.esWbSpawn,
		"_wbDefineRule":        engine.esWbDefineRule,
		"runRules":             engine.esWbRunRules,
		"readConfig":           engine.esReadConfig,
		"_wbPersistentSet":     engine.esPersistentSet,
		"_wbPersistentGet":     engine.esPersistentGet,
	})
	engine.globalCtx.GetPropString(-1, "log")
	engine.globalCtx.DefineFunctions(map[string]func(*ESContext) int{
		"debug":   engine.makeLogFunc(ENGINE_LOG_DEBUG),
		"info":    engine.makeLogFunc(ENGINE_LOG_INFO),
		"warning": engine.makeLogFunc(ENGINE_LOG_WARNING),
		"error":   engine.makeLogFunc(ENGINE_LOG_ERROR),
	})
	engine.globalCtx.Pop()

	// set global prototype to __wbModulePrototype
	engine.globalCtx.GetPropString(-1, MODULE_OBJ_PROTO_NAME)
	engine.globalCtx.SetPrototype(-2)
	// [ global ]

	if err := engine.loadLib(); err != nil {
		wbgo.Error.Panicf("failed to load runtime library: %s", err)
	}

	engine.globalCtx.Pop()
	// []

	// save global object in heap stash as __wbGlobalPrototype
	engine.globalCtx.PushHeapStash()
	engine.globalCtx.PushGlobalObject()
	// [ heap global ]

	engine.globalCtx.PutPropString(-2, GLOBAL_OBJ_PROTO_NAME)
	// [ heap ]

	engine.globalCtx.Pop()
	// []

	return
}

func (engine *ESEngine) exportModSearch(ctx *ESContext) {
	ctx.GetGlobalString("Duktape")
	ctx.PushGoFunc(func(c *duktape.Context) int {
		return engine.ModSearch(c)
	})
	ctx.PutPropString(-2, "modSearch")
	ctx.Pop()
}

func (engine *ESEngine) initHeapStashObject(name string, ctx *ESContext) {
	ctx.PushHeapStash()
	defer ctx.Pop()
	// [ stash ]

	ctx.PushObject()
	// [ stash object ]
	ctx.PutPropString(-2, name)
}

// initGlobalThreadList creates an object in heap stash to
// store thread objects
func (engine *ESEngine) initGlobalThreadList(ctx *ESContext) {
	engine.initHeapStashObject(THREAD_STORAGE_OBJ_NAME, ctx)
}

func (engine *ESEngine) initModulesStorage(ctx *ESContext) {
	engine.initHeapStashObject(MODULES_USER_STORAGE_OBJ_NAME, ctx)
}

func (engine *ESEngine) removeThreadFromStorage(ctx *ESContext, path string) {
	ctx.PushHeapStash()
	// [ stash ]

	ctx.GetPropString(-1, THREAD_STORAGE_OBJ_NAME)
	// [ stash threads ]
	defer ctx.Pop2()

	// try to get thread by name
	if ctx.HasPropString(-1, path) {
		ctx.DelPropString(-1, path)
	} else {
		wbgo.Error.Printf("trying to remove thread %s, but it doesn't exist", path)
	}
}

// initModulePrototype inits __wbModulePrototype object
// with methodes such as defineVirtualDevice etc.
func (engine *ESEngine) initModulePrototype(ctx *ESContext) {
	ctx.PushGlobalObject()
	defer ctx.Pop()

	ctx.PushObject()
	// [ global __wbModulePrototype ]

	ctx.DefineFunctions(map[string]func(*ESContext) int{
		"defineVirtualDevice": engine.esDefineVirtualDevice,
		"virtualDeviceId":     engine.esVirtualDeviceId,
		"_wbPersistentName":   engine.esPersistentName,
	})

	ctx.PutPropString(-2, MODULE_OBJ_PROTO_NAME)
}

// initVdevPrototype inits __wbVdevPrototype object - prototype
// for virtual device controllers
func (engine *ESEngine) initVdevPrototype(ctx *ESContext) {
	ctx.PushGlobalObject()
	defer ctx.Pop()

	ctx.PushObject()
	// [ global __wbVdevPrototype ]
	ctx.DefineFunctions(map[string]func(*ESContext) int{
		"getDeviceId": engine.esVdevGetDeviceId,
		"getCellId":   engine.esVdevGetCellId,
		// getCellValue and setCellValue are defined in lib.js
	})

	ctx.PutPropString(-2, "__wbVdevPrototype")
}

// Engine callback error handler
func (engine *ESEngine) CallbackErrorHandler(err ESError) {
	engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("ECMAScript error: %s", err))
}

func (engine *ESEngine) ScriptDir() string {
	// for Editor
	return engine.sourceRoot
}

func (engine *ESEngine) SetSourceRoot(sourceRoot string) (err error) {
	sourceRoot, err = filepath.Abs(sourceRoot)
	if err != nil {
		return
	}
	engine.sourceRoot = filepath.Clean(sourceRoot)
	return
}

func (engine *ESEngine) handleTimerCleanup(ctx *ESContext, timer TimerId) {
	var s *TimerSet
	var found = false

	// find timers set for current context
	if s, found = engine.ctxTimers[ctx]; !found {
		s = newTimerSet()
		engine.ctxTimers[ctx] = s
	}

	// register timer id
	s.timers[timer] = true

	// register cleanup handler
	engine.OnTimerRemoveByIndex(timer, func() {
		s.Lock()
		defer s.Unlock()
		delete(s.timers, timer)
	})
}

func (engine *ESEngine) runTimerCleanups(ctx *ESContext) {
	if s, found := engine.ctxTimers[ctx]; found {
		var ids = make([]TimerId, 0)

		// form timers list
		s.Lock()
		for id, active := range s.timers {
			if active {
				ids = append(ids, id)
			}
		}
		s.Unlock()

		// run cleanups
		for _, id := range ids {
			engine.StopTimerByIndex(id)
		}
	}
}

func (engine *ESEngine) buildSingleWhenChangedRuleCondition(ctx *ESContext, defIndex int) (RuleCondition, error) {
	if ctx.IsString(defIndex) {
		cellFullName := ctx.SafeToString(defIndex)
		parts := strings.SplitN(cellFullName, "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid whenChanged spec: '%s'", cellFullName)
		}
		return NewCellChangedRuleCondition(CellSpec{parts[0], parts[1]})
	}
	if ctx.IsFunction(defIndex) {
		f := ctx.WrapCallback(defIndex)
		return NewFuncValueChangedRuleCondition(func() interface{} { return f(nil) }), nil
	}
	return nil, errors.New("whenChanged: array expected")
}

func (engine *ESEngine) buildWhenChangedRuleCondition(ctx *ESContext, defIndex int) (RuleCondition, error) {
	ctx.GetPropString(defIndex, "whenChanged")
	defer ctx.Pop()

	if !ctx.IsArray(-1) {
		return engine.buildSingleWhenChangedRuleCondition(ctx, -1)
	}

	conds := make([]RuleCondition, ctx.GetLength(-1))

	for i := range conds {
		ctx.GetPropIndex(-1, uint(i))
		cond, err := engine.buildSingleWhenChangedRuleCondition(ctx, -1)
		ctx.Pop()
		if err != nil {
			return nil, err
		} else {
			conds[i] = cond
		}
	}

	return NewOrRuleCondition(conds), nil
}

func (engine *ESEngine) buildRuleCond(ctx *ESContext, defIndex int) (RuleCondition, error) {
	hasWhen := ctx.HasPropString(defIndex, "when")
	hasAsSoonAs := ctx.HasPropString(defIndex, "asSoonAs")
	hasWhenChanged := ctx.HasPropString(defIndex, "whenChanged")
	hasCron := ctx.HasPropString(defIndex, "_cron")

	switch {
	case hasWhen && (hasAsSoonAs || hasWhenChanged || hasCron):
		// _cron is added by lib.js. Under normal circumstances
		// it may not be combined with 'when' here, so no special message
		return nil, errors.New(
			"invalid rule -- cannot combine 'when' with 'asSoonAs', 'whenChanged' or 'cron'")

	case hasWhen:
		return NewLevelTriggeredRuleCondition(engine.wrapRuleCondFunc(ctx, defIndex, "when")), nil

	case hasAsSoonAs && (hasWhenChanged || hasCron):
		return nil, errors.New(
			"invalid rule -- cannot combine 'asSoonAs' with 'whenChanged' or 'cron'")

	case hasAsSoonAs:
		return NewEdgeTriggeredRuleCondition(
			engine.wrapRuleCondFunc(ctx, defIndex, "asSoonAs")), nil

	case hasWhenChanged && hasCron:
		return nil, errors.New("invalid rule -- cannot combine 'whenChanged' with cron spec")

	case hasWhenChanged:
		return engine.buildWhenChangedRuleCondition(ctx, defIndex)

	case hasCron:
		ctx.GetPropString(defIndex, "_cron")
		defer ctx.Pop()
		return NewCronRuleCondition(ctx.SafeToString(-1)), nil

	default:
		return nil, errors.New(
			"invalid rule -- must provide one of 'when', 'asSoonAs' or 'whenChanged'")
	}
}

func (engine *ESEngine) buildRule(ctx *ESContext, name string, defIndex int) (*Rule, error) {
	if !ctx.HasPropString(defIndex, "then") {
		// this should be handled by lib.js
		return nil, errors.New("invalid rule -- no then")
	}
	then := engine.wrapRuleCallback(ctx, defIndex, "then")
	if cond, err := engine.buildRuleCond(ctx, defIndex); err != nil {
		return nil, err
	} else {
		ruleId := engine.nextRuleId
		engine.nextRuleId++

		return NewRule(engine, ruleId, name, cond, then), nil
	}
}

func (engine *ESEngine) loadLib() error {
	for _, dir := range searchDirs {
		path := filepath.Join(dir, LIB_FILE)
		if _, err := os.Stat(path); err == nil {
			return engine.globalCtx.LoadScript(path)
		}
	}
	return noLibJs
}

func (engine *ESEngine) maybeRegisterSourceItem(ctx *ESContext, typ itemType, name string) {
	if engine.currentSource == nil {
		return
	}

	var items *[]LocItem
	switch typ {
	case SOURCE_ITEM_DEVICE:
		items = &engine.currentSource.Devices
	case SOURCE_ITEM_RULE:
		items = &engine.currentSource.Rules
	default:
		log.Panicf("bad source item type %d", typ)
	}

	line := -1
	for _, loc := range ctx.GetTraceback() {
		// Here we depend upon the fact that duktape displays
		// unmodified source paths in the backtrace
		if loc.filename == engine.currentSource.PhysicalPath {
			line = loc.line
		}
	}
	if line == -1 {
		return
	}
	*items = append(*items, LocItem{line, name})
}

func (engine *ESEngine) ListSourceFiles() (entries []LocFileEntry, err error) {
	engine.sourcesMtx.Lock()
	defer engine.sourcesMtx.Unlock()
	pathList := make([]string, 0, len(engine.sources))
	for virtualPath, _ := range engine.sources {
		pathList = append(pathList, virtualPath)
	}
	sort.Strings(pathList)
	entries = make([]LocFileEntry, len(pathList))
	for n, virtualPath := range pathList {
		entries[n] = *engine.sources[virtualPath]
	}
	return
}

func (engine *ESEngine) checkSourcePath(path string) (cleanPath string, virtualPath string, underSourceRoot bool, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return
	}

	cleanPath = filepath.Clean(path)
	if underSourceRoot = wbgo.IsSubpath(engine.sourceRoot, cleanPath); underSourceRoot {
		virtualPath, err = filepath.Rel(engine.sourceRoot, path)
	}

	return
}

func (engine *ESEngine) checkVirtualPath(path string) (cleanPath string, virtualPath string, err error) {
	physicalPath := filepath.Join(engine.sourceRoot, filepath.Clean(path))
	cleanPath, virtualPath, underSourceRoot, err := engine.checkSourcePath(physicalPath)
	if err == nil && !underSourceRoot {
		err = errors.New("path not under source root")
	}
	return
}

func (engine *ESEngine) LoadFile(path string) (err error) {
	_, err = engine.loadScript(path, true)
	return
}

func (engine *ESEngine) loadScript(path string, loadIfUnchanged bool) (bool, error) {
	path, virtualPath, underSourceRoot, err := engine.checkSourcePath(path)
	if err != nil {
		return false, err
	}

	if engine.currentSource != nil {
		// must use a stack of sources to support recursive LoadScript()
		panic("recursive loadScript() calls not supported")
	}

	wasChangedOrFirstSeen, err := engine.tracker.Track(virtualPath, path)
	if err != nil {
		return false, err
	}
	if !loadIfUnchanged && !wasChangedOrFirstSeen {
		wbgo.Debug.Printf("script %s unchanged, not reloading (possibly just reloaded)", path)
		return false, nil
	}

	// cleanup if old script exists
	engine.runCleanups(path)

	// prepare threads storage
	engine.globalCtx.PushHeapStash()
	// [ stash ]

	engine.globalCtx.GetPropString(-1, THREAD_STORAGE_OBJ_NAME)
	// [ stash threads ]

	// create new thread and context
	engine.globalCtx.PushThreadNewGlobalenv()
	// [ stash threads thread ]
	newLocalCtx := engine.ctxFactory.newESContextFromDuktape(engine.globalCtx.syncFunc, virtualPath, engine.globalCtx.GetContext(-1))
	// [ stash threads thread ]

	engine.localCtxs[path] = newLocalCtx

	// save new thread into storage
	engine.globalCtx.PutPropString(-2, path)
	// [ stash threads ]
	engine.globalCtx.Pop2()
	// []

	engine.cleanup.PushCleanupScope(path)
	defer engine.cleanup.PopCleanupScope(path)
	if underSourceRoot {
		engine.currentSource = &LocFileEntry{
			VirtualPath:  virtualPath,
			PhysicalPath: path,
			Devices:      make([]LocItem, 0),
			Rules:        make([]LocItem, 0),
		}
		engine.cleanup.AddCleanup(func() {
			engine.sourcesMtx.Lock()
			delete(engine.sources, virtualPath)
			engine.sourcesMtx.Unlock()
		})
		defer func() {
			engine.sourcesMtx.Lock()
			engine.sources[virtualPath] = engine.currentSource
			engine.sourcesMtx.Unlock()
			engine.currentSource = nil
		}()
	}

	// set error handler
	newLocalCtx.SetCallbackErrorHandler(engine.CallbackErrorHandler)

	// setup prototype for global object
	newLocalCtx.PushHeapStash()
	// [ stash ]
	newLocalCtx.PushGlobalObject()
	// [ stash global ]
	newLocalCtx.GetPropString(-2, GLOBAL_OBJ_PROTO_NAME)
	// [ stash global __wbGlobalProto ]

	// run initEnv function from prototype
	if newLocalCtx.HasPropString(-1, GLOBAL_INIT_ENV_FUNC_NAME) {
		newLocalCtx.GetPropString(-1, GLOBAL_INIT_ENV_FUNC_NAME)
		// [ ... initEnv ]
		newLocalCtx.PushGlobalObject()
		// [ ... initEnv global ]
		if newLocalCtx.Pcall(1) != 0 {
			wbgo.Error.Println("Failed to call __esInitEnv")
		}
		// [ ... ret ]
		newLocalCtx.Pop()
		// [ ... ]
	}

	newLocalCtx.SetPrototype(-2)

	// [ stash global ]

	newLocalCtx.Pop2()
	// []

	// export modSearch
	engine.exportModSearch(newLocalCtx)

	return true, engine.trackESError(path, newLocalCtx.LoadScenario(path))
}

func (engine *ESEngine) trackESError(path string, err error) error {
	esError, ok := err.(ESError)
	if !ok {
		return err
	}

	// ESError contains physical file paths in its traceback.
	// Here we need to translate them to virtual paths.
	// We skip any frames that refer to files that don't
	// reside under the source root.
	traceback := make([]LocItem, 0, len(esError.Traceback))
	for _, esLoc := range esError.Traceback {
		_, virtualPath, underSourceRoot, err :=
			engine.checkSourcePath(esLoc.filename)
		if err == nil && underSourceRoot {
			traceback = append(traceback, LocItem{esLoc.line, virtualPath})
		}
	}

	scriptErr := NewScriptError(esError.Message, traceback)
	if engine.currentSource != nil {
		engine.currentSource.Error = &scriptErr
	}
	return scriptErr
}

func (engine *ESEngine) maybePublishUpdate(subtopic, physicalPath string) {
	_, virtualPath, underSourceRoot, err := engine.checkSourcePath(physicalPath)
	if err != nil {
		wbgo.Error.Printf("checkSourcePath() failed for %s: %s", physicalPath, err)
	}
	if underSourceRoot {
		engine.Publish("/wbrules/updates/"+subtopic, virtualPath, 1, false)
	}
}

func (engine *ESEngine) runCleanups(path string) {
	// run context cleanups
	// try to get local context for this script
	if _, ok := engine.localCtxs[path]; ok {
		wbgo.Debug.Printf("local context for script %s exists; removing it", path)

		// cleanup timers of this context
		engine.runTimerCleanups(engine.localCtxs[path])

		// TODO: launch internal cleanups
		engine.removeThreadFromStorage(engine.globalCtx, path)
	}

	// run rules cleanups
	engine.cleanup.RunCleanups(path)
}

func (engine *ESEngine) loadScriptAndRefresh(path string, loadIfUnchanged bool) (err error) {
	loaded, err := engine.loadScript(path, loadIfUnchanged)
	if loaded {
		// must call refresh() even in case of loadScript() error,
		// because a part of script was still probably loaded
		engine.Refresh()
		engine.maybePublishUpdate("changed", path)
	}
	return
}

func (engine *ESEngine) LiveWriteScript(virtualPath, content string) error {
	r := make(chan error)
	engine.model.WhenReady(func() {
		wbgo.Debug.Printf("OverwriteScript(%s)", virtualPath)
		cleanPath, virtualPath, err := engine.checkVirtualPath(virtualPath)
		wbgo.Debug.Printf("OverwriteScript: %s %s %v", cleanPath, virtualPath, err)
		if err != nil {
			r <- err
			return
		}

		// Make sure directories that contain the script exist
		if strings.Contains(virtualPath, "/") {
			if err = os.MkdirAll(filepath.Dir(cleanPath), 0777); err != nil {
				wbgo.Error.Printf("error making dirs for %s: %s", cleanPath, err)
				r <- err
				return
			}
		}

		// WriteFile() will cause DirWatcher to wake up and invoke
		// LiveLoadFile for the file, but as the new content
		// will be already registered with the contentTracker,
		// duplicate reload will not happen
		err = ioutil.WriteFile(cleanPath, []byte(content), 0777)
		if err != nil {
			r <- err
			return
		}
		r <- engine.loadScriptAndRefresh(cleanPath, true)
	})
	return <-r
}

// LiveLoadFile loads the specified script in the running engine.
// If the engine isn't ready yet, the function waits for it to become
// ready. If the script didn't change since the last time it was loaded,
// the script isn't loaded.
func (engine *ESEngine) LiveLoadFile(path string) error {
	r := make(chan error)
	engine.model.WhenReady(func() {
		r <- engine.loadScriptAndRefresh(path, false)
	})

	return <-r
}

func (engine *ESEngine) LiveRemoveFile(path string) error {
	engine.model.WhenReady(func() {
		engine.runCleanups(path)
		engine.Refresh()
		engine.maybePublishUpdate("removed", path)
	})
	return nil
}

func (engine *ESEngine) wrapRuleCallback(ctx *ESContext, defIndex int, propName string) ESCallbackFunc {
	ctx.GetPropString(defIndex, propName)
	defer ctx.Pop()
	return ctx.WrapCallback(-1)
}

func (engine *ESEngine) wrapRuleCondFunc(ctx *ESContext, defIndex int, defProp string) func() bool {
	f := engine.wrapRuleCallback(ctx, defIndex, defProp)
	return func() bool {
		r, ok := f(nil).(bool)
		return ok && r
	}
}

func getFilenameHash(filename string) string {
	if result, ok := filenameMd5s[filename]; ok {
		return result
	} else {
		// TODO: TBD: detect collisions on current configuration?
		hash := md5.Sum([]byte(filename))

		// reduce hash length to 32
		for i := 0; i < md5.Size/4; i++ {
			hash[i] = hash[i] ^ hash[md5.Size/4+i] ^ hash[md5.Size/2+i] ^ hash[md5.Size*3/4+i]
		}

		result = base64.RawURLEncoding.EncodeToString(hash[:md5.Size/4])
		filenameMd5s[filename] = result

		return result
	}
}

// localObjectId generates global-unique object ID
// for local one according to module file name.
// Used in defineVirtualDevice and PersistentStorage
func localObjectId(filename, objname string) string {
	hash := getFilenameHash(filename)
	return "_" + hash + objname
}

// maybeExpandLocalObjectId converts local object ID to global.
// Local object is an object created in 'module' scope
// (e.g. by module.defineVirtualDevice()).
// This method should be called only from exported functions
// in __wbModulePrototype child context
func (engine *ESEngine) maybeExpandLocalObjectId(ctx *ESContext, name string) string {
	ctx.PushThis()
	if ctx.IsObject(-1) && ctx.HasPropString(-1, "filename") {
		// this means we are in some local scope
		// so, replace virtual device name
		ctx.GetPropString(-1, "filename")

		if ctx.IsString(-1) {
			name = localObjectId(ctx.GetString(-1), name)
		}
		ctx.Pop()
	}
	ctx.Pop()

	return name
}

// getStringPropFromObject gets string property value from object
func (engine *ESEngine) getStringPropFromObject(ctx *ESContext, objIndex int, propName string) (id string, err error) {
	// [ ... obj ... ]

	if !ctx.HasPropString(objIndex, propName) {
		err = noSuchPropError
		return
	}

	ctx.GetPropString(objIndex, propName)
	defer ctx.Pop()
	// [ ... obj ... prop ]

	id = ctx.GetString(-1)

	if id == "" {
		err = wrongPropTypeError
		return
	}

	return
}

// esVirtualDeviceId exported as module.virtualDeviceId(name)
// and allows user to get global ID for local device
func (engine *ESEngine) esVirtualDeviceId(ctx *ESContext) int {
	// arguments:
	// 1 -> deviceName
	if ctx.GetTop() != 1 || !ctx.IsString(-1) {
		return duktape.DUK_RET_ERROR
	}

	name := ctx.GetString(-1)
	name = engine.maybeExpandLocalObjectId(ctx, name) // TODO: ctx

	// push result
	ctx.PushString(name)

	return 1
}

// defineVirtualDevice creates virtual device object in MQTT
// and returns JS object to control it
func (engine *ESEngine) esDefineVirtualDevice(ctx *ESContext) int {
	if ctx.GetTop() != 2 || !ctx.IsString(-2) || !ctx.IsObject(-1) {
		return duktape.DUK_RET_ERROR
	}
	name := ctx.GetString(-2)
	obj := ctx.GetJSObject(-1).(objx.Map)

	name = engine.maybeExpandLocalObjectId(ctx, name)

	if err := engine.DefineVirtualDevice(name, obj); err != nil {
		wbgo.Error.Printf("device definition error: %s", err)
		ctx.PushErrorObject(duktape.DUK_ERR_ERROR, err.Error())
		return duktape.DUK_RET_INSTACK_ERROR
	}
	engine.maybeRegisterSourceItem(ctx, SOURCE_ITEM_DEVICE, name)

	// [ args | ]

	// create virtual device object
	ctx.PushObject()
	// [ args | vDevObject ]

	// get prototype

	// get global object first
	ctx.PushGlobalObject()
	// [ args | vDevObject global ]

	// get prototype object
	ctx.GetPropString(-1, VDEV_OBJ_PROTO_NAME)
	// [ args | vDevObject global __wbVdevPrototype ]

	// apply prototype
	ctx.SetPrototype(-3)
	// [ args | vDevObject global ]

	ctx.Pop()
	// [ args | vDevObject ]

	// push device ID property

	ctx.PushString(name)
	// [ args | vDevObject devId ]

	ctx.PutPropString(-2, VDEV_OBJ_PROP_DEVID)
	// [ args | vDevObject ]

	return 1
}

// esVdevGetDeviceId returns virtual device ID string (for MQTT)
// from virtual device object
// Exported to JS as method of virtual device object
func (engine *ESEngine) esVdevGetDeviceId(ctx *ESContext) int {
	// this -> virtual device object
	// no arguments
	if ctx.GetTop() != 0 {
		return duktape.DUK_RET_ERROR
	}

	ctx.PushThis()
	// [ this ]

	// get virtual device id
	devId, err := engine.getStringPropFromObject(ctx, -1, VDEV_OBJ_PROP_DEVID)
	if err != nil {
		ctx.Pop()
		// []

		return duktape.DUK_RET_TYPE_ERROR
	}

	ctx.Pop()
	// []

	// return id
	ctx.PushString(devId)
	// [ id ]

	return 1
}

// esVdevGetCellId returns virtual device cell ID string
// in 'dev/cell' form from virtual device object
// Exported to JS as method of virtual device object
// Arguments:
// * cell -> cell name
func (engine *ESEngine) esVdevGetCellId(ctx *ESContext) int {
	// this -> virtual device object
	// arguments:
	// 1 -> cell
	//
	// [ cell | ]

	if ctx.GetTop() != 1 || !ctx.IsString(-1) {
		return duktape.DUK_RET_ERROR
	}

	cellId := ctx.GetString(-1)

	// push this
	ctx.PushThis()
	// [ cell | this ]

	// get virtual device id
	devId, err := engine.getStringPropFromObject(ctx, -1, VDEV_OBJ_PROP_DEVID)
	if err != nil {
		ctx.Pop()
		// [ cell | ]

		return duktape.DUK_RET_TYPE_ERROR
	}

	ctx.Pop()
	// [ cell | ]

	cellId = devId + "/" + cellId

	ctx.PushString(cellId)
	// [ cell | cellId ]

	return 1
}

func (engine *ESEngine) esFormat(ctx *ESContext) int {
	ctx.PushString(ctx.Format())
	return 1
}

func (engine *ESEngine) makeLogFunc(level EngineLogLevel) func(ctx *ESContext) int {
	return func(ctx *ESContext) int {
		engine.Log(level, ctx.Format())
		return 0
	}
}

func (engine *ESEngine) esPublish(ctx *ESContext) int {
	retain := false
	qos := 0
	if ctx.GetTop() == 4 {
		retain = ctx.ToBoolean(-1)
		ctx.Pop()
	}
	if ctx.GetTop() == 3 {
		qos = int(ctx.ToNumber(-1))
		ctx.Pop()
		if qos < 0 || qos > 2 {
			return duktape.DUK_RET_ERROR
		}
	}
	if ctx.GetTop() != 2 {
		return duktape.DUK_RET_ERROR
	}
	if !ctx.IsString(-2) {
		return duktape.DUK_RET_TYPE_ERROR
	}
	topic := ctx.GetString(-2)
	payload := ctx.SafeToString(-1)
	engine.Publish(topic, payload, byte(qos), retain)
	return 0
}

func (engine *ESEngine) esWbDevObject(ctx *ESContext) int {
	wbgo.Debug.Printf("esWbDevObject(): top=%d isString=%v", ctx.GetTop(), ctx.IsString(-1))
	if ctx.GetTop() != 1 || !ctx.IsString(-1) {
		return duktape.DUK_RET_ERROR
	}
	devProxy := engine.GetDeviceProxy(ctx.GetString(-1))
	ctx.PushGoObject(devProxy)
	return 1
}

func (engine *ESEngine) esWbCellObject(ctx *ESContext) int {
	if ctx.GetTop() != 2 || !ctx.IsString(-1) || !ctx.IsObject(-2) {
		return duktape.DUK_RET_ERROR
	}
	devProxy, ok := ctx.GetGoObject(-2).(*DeviceProxy)
	if !ok {
		wbgo.Error.Printf("invalid _wbCellObject call")
		return duktape.DUK_RET_TYPE_ERROR
	}
	cellProxy := devProxy.EnsureCell(ctx.GetString(-1))
	ctx.PushGoObject(cellProxy)
	ctx.DefineFunctions(map[string]func(*ESContext) int{
		"rawValue": func(ctx *ESContext) int {
			ctx.PushString(cellProxy.RawValue())
			return 1
		},
		"value": func(ctx *ESContext) int {
			m := objx.New(map[string]interface{}{
				"v": cellProxy.Value(),
			})
			ctx.PushJSObject(m)
			return 1
		},
		"setValue": func(ctx *ESContext) int {
			if ctx.GetTop() != 1 || !ctx.IsObject(-1) {
				return duktape.DUK_RET_ERROR
			}
			m, ok := ctx.GetJSObject(-1).(objx.Map)
			if !ok || !m.Has("v") {
				wbgo.Error.Printf("invalid cell definition")
				return duktape.DUK_RET_TYPE_ERROR
			}
			cellProxy.SetValue(m["v"])
			return 1
		},
		"isComplete": func(ctx *ESContext) int {
			ctx.PushBoolean(cellProxy.IsComplete())
			return 1
		},
	})
	return 1
}

func (engine *ESEngine) esWbStartTimer(ctx *ESContext) int {
	if ctx.GetTop() != 3 || !ctx.IsNumber(1) {
		// FIXME: need to throw proper exception here
		wbgo.Error.Println("bad _wbStartTimer call")
		return duktape.DUK_RET_ERROR
	}

	name := NO_TIMER_NAME
	if ctx.IsString(0) {
		name = ctx.ToString(0)
		if name == "" {
			wbgo.Error.Println("empty timer name")
			return duktape.DUK_RET_ERROR
		}
		engine.StopTimerByName(name)
	} else if !ctx.IsFunction(0) {
		wbgo.Error.Println("invalid timer spec")
		return duktape.DUK_RET_ERROR
	}

	ms := ctx.GetNumber(1)
	if ms < MIN_INTERVAL_MS {
		ms = MIN_INTERVAL_MS
	}
	periodic := ctx.ToBoolean(2)

	var callback func()
	if name == NO_TIMER_NAME {
		f := ctx.WrapCallback(0)
		callback = func() { f(nil) }
	}

	interval := time.Duration(ms * float64(time.Millisecond))

	// get timer id
	timerId := engine.StartTimer(name, callback, interval, periodic)

	// add timer to script cleanup
	engine.handleTimerCleanup(ctx, timerId)

	ctx.PushNumber(float64(timerId))
	return 1
}

func (engine *ESEngine) esWbStopTimer(ctx *ESContext) int {
	if ctx.GetTop() != 1 {
		return duktape.DUK_RET_ERROR
	}
	if ctx.IsNumber(0) {
		n := TimerId(ctx.GetNumber(-1))
		if n == 0 {
			wbgo.Error.Printf("timer id cannot be zero")
			return 0
		}
		engine.StopTimerByIndex(n)
	} else if ctx.IsString(0) {
		engine.StopTimerByName(ctx.ToString(0))
	} else {
		return duktape.DUK_RET_ERROR
	}
	return 0
}

func (engine *ESEngine) esWbCheckCurrentTimer(ctx *ESContext) int {
	if ctx.GetTop() != 1 || !ctx.IsString(0) {
		return duktape.DUK_RET_ERROR
	}
	timerName := ctx.ToString(0)
	ctx.PushBoolean(engine.CheckTimer(timerName))
	return 1
}

func (engine *ESEngine) esWbSpawn(ctx *ESContext) int {
	if ctx.GetTop() != 5 || !ctx.IsArray(0) || !ctx.IsBoolean(2) ||
		!ctx.IsBoolean(3) {
		return duktape.DUK_RET_ERROR
	}

	args := ctx.StringArrayToGo(0)
	if len(args) == 0 {
		return duktape.DUK_RET_ERROR
	}

	callbackFn := ESCallbackFunc(nil)

	if ctx.IsFunction(1) {
		callbackFn = ctx.WrapCallback(1)
	} else if !ctx.IsNullOrUndefined(1) {
		return duktape.DUK_RET_ERROR
	}

	var input *string
	if ctx.IsString(4) {
		instr := ctx.GetString(4)
		input = &instr
	} else if !ctx.IsNullOrUndefined(4) {
		return duktape.DUK_RET_ERROR
	}

	captureOutput := ctx.GetBoolean(2)
	captureErrorOutput := ctx.GetBoolean(3)
	go func() {
		r, err := Spawn(args[0], args[1:], captureOutput, captureErrorOutput, input)
		if err != nil {
			wbgo.Error.Printf("external command failed: %s", err)
			return
		}
		if callbackFn != nil {
			engine.model.CallSync(func() {
				args := objx.New(map[string]interface{}{
					"exitStatus": r.ExitStatus,
				})
				if captureOutput {
					args["capturedOutput"] = r.CapturedOutput
				}
				args["capturedErrorOutput"] = r.CapturedErrorOutput
				callbackFn(args)
			})
		} else if r.ExitStatus != 0 {
			wbgo.Error.Printf("command '%s' failed with exit status %d",
				strings.Join(args, " "), r.ExitStatus)
		}
	}()
	return 0
}

func (engine *ESEngine) esWbDefineRule(ctx *ESContext) int {
	var ok = false
	var shortName, name string
	var objIndex int

	switch ctx.GetTop() {
	case 1:
		if ctx.IsObject(0) {
			objIndex = 0
			ok = true
		}
	case 2:
		if ctx.IsString(0) && ctx.IsObject(1) {
			objIndex = 1

			shortName = ctx.GetString(0)
			name = ctx.GetCurrentFilename() + "/" + shortName
			// if engine.currentSource != nil {
			// name = engine.currentSource.VirtualPath + "/" + shortName
			// }

			ok = true
		}
	}
	if !ok {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("bad rule definition"))
		return duktape.DUK_RET_ERROR
	}

	var rule *Rule
	var err error
	var ruleId RuleId

	if rule, err = engine.buildRule(ctx, name, objIndex); err != nil {
		// FIXME: proper error handling
		engine.Log(ENGINE_LOG_ERROR,
			fmt.Sprintf("bad definition of rule '%s': %s", name, err))
		return duktape.DUK_RET_ERROR
	}

	if ruleId, err = engine.DefineRule(rule); err != nil {
		engine.Log(ENGINE_LOG_ERROR,
			fmt.Sprintf("defineRule error: %s", err))
		return duktape.DUK_RET_ERROR
	}

	engine.maybeRegisterSourceItem(ctx, SOURCE_ITEM_RULE, shortName)

	// return rule ID
	ctx.PushNumber(float64(ruleId))
	return 1
}

func (engine *ESEngine) esWbRunRules(ctx *ESContext) int {
	switch ctx.GetTop() {
	case 0:
		engine.RunRules(nil, NO_TIMER_NAME)
	case 2:
		devName := ctx.SafeToString(0)
		cellName := ctx.SafeToString(1)
		engine.RunRules(&CellSpec{devName, cellName}, NO_TIMER_NAME)
	default:
		return duktape.DUK_RET_ERROR
	}
	return 0
}

func (engine *ESEngine) esReadConfig(ctx *ESContext) int {
	if ctx.GetTop() != 1 || !ctx.IsString(0) {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("invalid readConfig call"))
		return duktape.DUK_RET_ERROR
	}
	path := ctx.GetString(0)
	in, err := os.Open(path)
	if err != nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("failed to open config file: %s", path))
		return duktape.DUK_RET_ERROR
	}
	defer in.Close()

	reader := JsonConfigReader.New(in)
	preprocessedContent, err := ioutil.ReadAll(reader)
	if err != nil {
		// JsonConfigReader doesn't produce its own errors, thus
		// any errors returned from it are I/O errors.
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("failed to read config file: %s", path))
		return duktape.DUK_RET_ERROR
	}

	parsedJSON, err := objx.FromJSON(string(preprocessedContent))
	if err != nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("failed to parse json: %s", path))
		return duktape.DUK_RET_ERROR
	}
	ctx.PushJSObject(parsedJSON)
	return 1
}

func (engine *ESEngine) EvalScript(code string) error {
	ch := make(chan error)
	engine.model.CallSync(func() {
		err := engine.globalCtx.EvalScript(code)
		if err != nil {
			engine.Logf(ENGINE_LOG_ERROR, "eval error: %s", err)
		}
		ch <- err
	})
	return <-ch
}

// Persistent storage features

// Create or open DB file
func (engine *ESEngine) SetPersistentDB(filename string) error {
	return engine.SetPersistentDBMode(filename, PERSISTENT_DB_CHMOD)
}

func (engine *ESEngine) SetPersistentDBMode(filename string, mode os.FileMode) (err error) {
	if engine.persistentDB != nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent storage DB is already opened"))
		err = fmt.Errorf("persistent storage DB is already opened")
		return
	}

	engine.persistentDB, err = bolt.Open(filename, mode,
		&bolt.Options{Timeout: 1 * time.Second})

	if err != nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("can't open persistent DB file: %s", err))
		return
	}

	return nil
}

// Force close DB
func (engine *ESEngine) ClosePersistentDB() (err error) {
	if engine.persistentDB == nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("DB is not opened, nothing to close"))
		err = fmt.Errorf("nothing to close")
		return
	}

	err = engine.persistentDB.Close()

	return
}

// Creates a name for persistent storage bucket.
// Used in 'module.PersistentStorage(name, options)'
func (engine *ESEngine) esPersistentName(ctx *ESContext) int {

	// panic(fmt.Sprintf("run esPersistentName at context %p", ctx))

	if engine.persistentDB == nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent DB is not initialized"))
		return duktape.DUK_RET_ERROR
	}

	// arguments: (name [, options = { global bool }])
	var name string

	numArgs := ctx.GetTop()

	if numArgs < 1 || numArgs > 2 {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("bad persistent storage definition"))
		return duktape.DUK_RET_ERROR
	}

	// parse name
	if !ctx.IsString(0) {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent storage name must be string"))
		return duktape.DUK_RET_ERROR
	}
	name = ctx.GetString(0)

	// parse options object
	if numArgs == 2 && !ctx.IsUndefined(1) {
		if !ctx.IsObject(1) {
			engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent storage options must be object"))
			return duktape.DUK_RET_ERROR
		}
	}

	// get global ID for bucket if this is local storage
	name = engine.maybeExpandLocalObjectId(ctx, name)
	engine.Log(ENGINE_LOG_DEBUG, fmt.Sprintf("create local storage name: %s", name))

	// push name as return value
	ctx.PushString(name)

	return 1
}

// Writes new value down to persistent DB
func (engine *ESEngine) esPersistentSet(ctx *ESContext) int {
	if engine.persistentDB == nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent DB is not initialized"))
		return duktape.DUK_RET_ERROR
	}

	// arguments: (bucket string, key string, value)
	var bucket, key, value string

	if ctx.GetTop() != 3 {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("bad persistentSet request, arg number mismatch"))
		return duktape.DUK_RET_ERROR
	}

	// parse bucket name
	if !ctx.IsString(0) {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent storage bucket name must be string"))
		return duktape.DUK_RET_ERROR
	}
	bucket = ctx.GetString(0)

	// parse key
	if !ctx.IsString(1) {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent storage key must be string"))
		return duktape.DUK_RET_ERROR
	}
	key = ctx.GetString(1)

	// parse value
	value = ctx.JsonEncode(2)

	// perform a transaction
	engine.persistentDB.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}

		if err := b.Put([]byte(key), []byte(value)); err != nil {
			return err
		}
		return nil
	})

	wbgo.Debug.Printf("write value to persistent storage %s: '%s' <= '%s'", bucket, key, value)

	return 0
}

// Gets a value from persitent DB
func (engine *ESEngine) esPersistentGet(ctx *ESContext) int {
	if engine.persistentDB == nil {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent DB is not initialized"))
		return duktape.DUK_RET_ERROR
	}

	// arguments: (bucket string, key string)
	var bucket, key, value string

	if ctx.GetTop() != 2 {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("bad persistentGet request, arg number mismatch"))
		return duktape.DUK_RET_ERROR
	}

	// parse bucket name
	if !ctx.IsString(0) {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent storage bucket name must be string"))
		return duktape.DUK_RET_ERROR
	}
	bucket = ctx.GetString(0)

	// parse key
	if !ctx.IsString(1) {
		engine.Log(ENGINE_LOG_ERROR, fmt.Sprintf("persistent storage key must be string"))
		return duktape.DUK_RET_ERROR
	}
	key = ctx.GetString(1)

	wbgo.Debug.Printf("trying to get value from persistent storage %s: %s", bucket, key)

	// try to get these from cache
	var ok bool
	// read value
	engine.persistentDB.View(func(tx *bolt.Tx) error {
		ok = false
		b := tx.Bucket([]byte(bucket))
		if b == nil { // no such bucket -> undefined
			return nil
		}
		if v := b.Get([]byte(key)); v != nil {
			value = string(v)
			ok = true
		}
		return nil
	})

	if !ok {
		// push 'undefined'
		ctx.PushUndefined()
	} else {
		// push value into stack and decode JSON
		ctx.PushString(value)
		ctx.JsonDecode(-1)
	}

	return 1
}

// native modSearch implementation
func (engine *ESEngine) ModSearch(ctx *duktape.Context) int {
	// arguments:
	// 0: id
	// 1: require
	// 2: exports
	// 3: module

	// get module name (id)
	id := ctx.GetString(0)
	wbgo.Debug.Printf("[modsearch] required module %s", id)

	// try to find this module in directory
	for _, dir := range engine.modulesDirs {
		path := dir + "/" + id + ".js"
		wbgo.Debug.Printf("[modsearch] trying to read file %s", path)

		// TBD: something external to load scripts properly
		// now just try to read file
		src, err := ioutil.ReadFile(path)

		if err == nil {
			wbgo.Debug.Printf("[modsearch] file found!")

			// set module properties
			// put module.filename
			ctx.PushString(path)
			// [ args | path ]
			ctx.PutPropString(3, MODULE_FILENAME_PROP)
			// [ args | ]

			// put module.storage
			ctx.PushHeapStash()
			// [ args | heapStash ]
			ctx.GetPropString(-1, MODULES_USER_STORAGE_OBJ_NAME)
			// [ args | heapStash _esModules ]

			// check if storage for this module is allocated
			if !ctx.HasPropString(-1, path) {
				// create storage
				ctx.PushObject()
				// [ args | heapStash _esModules newStorage ]
				ctx.PutPropString(-2, path)
				// [ args | heapStash _esModules ]
			}
			// add this storage to module
			ctx.GetPropString(-1, path)
			// [ args | heapStash _esModules storage ]
			ctx.PutPropString(3, MODULE_STORAGE_PROP)
			// [ args | heapStash _esModules ]
			ctx.Pop2()
			// [ args | ]

			// return module sources
			ctx.PushString(string(src))

			return 1
		}
	}

	wbgo.Error.Printf("error requiring module %s, not found", id)

	return duktape.DUK_RET_ERROR
}
