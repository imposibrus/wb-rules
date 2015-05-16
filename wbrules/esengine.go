package wbrules

import (
	"errors"
	"fmt"
	"github.com/GeertJohan/go.rice"
	wbgo "github.com/contactless/wbgo"
	duktape "github.com/ivan4th/go-duktape"
	"github.com/stretchr/objx"
	"strings"
	"time"
)

const (
	LIB_FILE = "lib.js"
)

type ESEngine struct {
	*RuleEngine
	ctx       *ESContext
	scriptBox *rice.Box
}

func NewESEngine(model *CellModel, mqttClient wbgo.MQTTClient) (engine *ESEngine) {
	engine = &ESEngine{
		NewRuleEngine(model, mqttClient),
		newESContext(),
		rice.MustFindBox("scripts"),
	}

	engine.ctx.PushGlobalObject()
	engine.ctx.DefineFunctions(map[string]func() int{
		"defineVirtualDevice":  engine.esDefineVirtualDevice,
		"format":               engine.esFormat,
		"log":                  engine.esLog,
		"debug":                engine.esDebug,
		"publish":              engine.esPublish,
		"_wbDevObject":         engine.esWbDevObject,
		"_wbCellObject":        engine.esWbCellObject,
		"_wbStartTimer":        engine.esWbStartTimer,
		"_wbStopTimer":         engine.esWbStopTimer,
		"_wbCheckCurrentTimer": engine.esWbCheckCurrentTimer,
		"_wbSpawn":             engine.esWbSpawn,
		"_wbDefineRule":        engine.esWbDefineRule,
		"runRules":             engine.esWbRunRules,
	})
	engine.ctx.Pop()
	if err := engine.loadLib(); err != nil {
		wbgo.Error.Panicf("failed to load runtime library: %s", err)
	}
	return
}

func (engine *ESEngine) buildSingleWhenChangedRuleCondition(defIndex int) (RuleCondition, error) {
	if engine.ctx.IsString(defIndex) {
		cellFullName := engine.ctx.SafeToString(defIndex)
		parts := strings.SplitN(cellFullName, "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid whenChanged spec: '%s'", cellFullName)
		}
		return newCellChangedRuleCondition(engine.getCell(parts[0], parts[1]))
	}
	if engine.ctx.IsFunction(defIndex) {
		f := engine.ctx.WrapCallback(defIndex)
		return newFuncValueChangedRuleCondition(func() interface{} { return f(nil) }), nil
	}
	return nil, errors.New("whenChanged: array expected")
}

func (engine *ESEngine) buildWhenChangedRuleCondition(defIndex int) (RuleCondition, error) {
	ctx := engine.ctx
	ctx.GetPropString(defIndex, "whenChanged")
	defer ctx.Pop()

	if !ctx.IsArray(-1) {
		return engine.buildSingleWhenChangedRuleCondition(-1)
	}

	conds := make([]RuleCondition, ctx.GetLength(-1))

	for i := range conds {
		ctx.GetPropIndex(-1, uint(i))
		cond, err := engine.buildSingleWhenChangedRuleCondition(-1)
		ctx.Pop()
		if err != nil {
			return nil, err
		} else {
			conds[i] = cond
		}
	}

	return newOrRuleCondition(conds), nil
}

func (engine *ESEngine) buildRuleCond(defIndex int) (RuleCondition, error) {
	ctx := engine.ctx
	hasWhen := ctx.HasPropString(defIndex, "when")
	hasAsSoonAs := ctx.HasPropString(defIndex, "asSoonAs")
	hasWhenChanged := ctx.HasPropString(defIndex, "whenChanged")
	hasCron := ctx.HasPropString(defIndex, "_cron")

	switch {
	case hasWhen && (hasAsSoonAs || hasWhenChanged || hasCron):
		// _cron is added by lib.js. Under normal circumstances
		// it may not be combined with 'when' here, so no special message
		return nil, errors.New(
			"invalid rule -- cannot combine 'when' with 'asSoonAs' or 'whenChanged'")

	case hasWhen:
		return newLevelTriggeredRuleCondition(engine.wrapRuleCondFunc(defIndex, "when")), nil

	case hasAsSoonAs && (hasWhenChanged || hasCron):
		return nil, errors.New(
			"invalid rule -- cannot combine 'asSoonAs' with 'whenChanged'")

	case hasAsSoonAs:
		return newEdgeTriggeredRuleCondition(
			engine.wrapRuleCondFunc(defIndex, "asSoonAs")), nil

	case hasWhenChanged && hasCron:
		return nil, errors.New("invalid rule -- cannot combine 'whenChanged' with cron spec")

	case hasWhenChanged:
		return engine.buildWhenChangedRuleCondition(defIndex)

	case hasCron:
		engine.ctx.GetPropString(defIndex, "_cron")
		defer engine.ctx.Pop()
		return newCronRuleCondition(engine.ctx.SafeToString(-1)), nil

	default:
		return nil, errors.New(
			"invalid rule -- must provide one of 'when', 'asSoonAs' or 'whenChanged'")
	}
}

func (engine *ESEngine) buildRule(name string, defIndex int) (*Rule, error) {
	if !engine.ctx.HasPropString(defIndex, "then") {
		// this should be handled by lib.js
		return nil, errors.New("invalid rule -- no then")
	}
	then := engine.wrapRuleCallback(defIndex, "then")
	if cond, err := engine.buildRuleCond(defIndex); err != nil {
		return nil, err
	} else {
		return NewRule(engine, name, cond, then), nil
	}
}

func (engine *ESEngine) loadLib() error {
	libStr, err := engine.scriptBox.String(LIB_FILE)
	if err != nil {
		return err
	}
	return engine.ctx.LoadEmbeddedScript(LIB_FILE, libStr)
}

func (engine *ESEngine) LoadScript(path string) error {
	return engine.ctx.LoadScript(path)
}

// LiveLoadScript loads the specified script in the running engine.
// If the engine isn't ready yet, the function waits for it to become
// ready.
func (engine *ESEngine) LiveLoadScript(path string) error {
	r := make(chan error)
	engine.model.WhenReady(func() {
		engine.model.CallSync(func() {
			err := engine.LoadScript(path)
			// must reload cron rules even in case of LoadScript() error,
			// because a part of script was still probably loaded
			engine.setupCron()
			r <- err
		})
	})

	return <-r
}

func (engine *ESEngine) wrapRuleCallback(defIndex int, propName string) ESCallbackFunc {
	engine.ctx.GetPropString(defIndex, propName)
	defer engine.ctx.Pop()
	return engine.ctx.WrapCallback(-1)
}

func (engine *ESEngine) wrapRuleCondFunc(defIndex int, defProp string) func() bool {
	f := engine.wrapRuleCallback(defIndex, defProp)
	return func() bool {
		r, ok := f(nil).(bool)
		return ok && r
	}
}

func (engine *ESEngine) esDefineVirtualDevice() int {
	if engine.ctx.GetTop() != 2 || !engine.ctx.IsString(-2) || !engine.ctx.IsObject(-1) {
		return duktape.DUK_RET_ERROR
	}
	name := engine.ctx.GetString(-2)
	obj := engine.ctx.GetJSObject(-1).(objx.Map)
	if err := engine.defineVirtualDevice(name, obj); err != nil {
		wbgo.Error.Printf("device definition error: %s", err)
		return duktape.DUK_RET_ERROR
	}
	return 0
}

func (engine *ESEngine) esFormat() int {
	engine.ctx.PushString(engine.ctx.Format())
	return 1
}

func (engine *ESEngine) esLog() int {
	engine.logFunc(engine.ctx.Format())
	return 0
}

func (engine *ESEngine) esDebug() int {
	wbgo.Debug.Printf("[rule debug] %s", engine.ctx.Format())
	return 0
}

func (engine *ESEngine) esPublish() int {
	retain := false
	qos := 0
	if engine.ctx.GetTop() == 4 {
		retain = engine.ctx.ToBoolean(-1)
		engine.ctx.Pop()
	}
	if engine.ctx.GetTop() == 3 {
		qos = int(engine.ctx.ToNumber(-1))
		engine.ctx.Pop()
		if qos < 0 || qos > 2 {
			return duktape.DUK_RET_ERROR
		}
	}
	if engine.ctx.GetTop() != 2 {
		return duktape.DUK_RET_ERROR
	}
	if !engine.ctx.IsString(-2) {
		return duktape.DUK_RET_TYPE_ERROR
	}
	topic := engine.ctx.GetString(-2)
	payload := engine.ctx.SafeToString(-1)
	engine.publish(topic, payload, byte(qos), retain)
	return 0
}

func (engine *ESEngine) esWbDevObject() int {
	wbgo.Debug.Printf("esWbDevObject(): top=%d isString=%v", engine.ctx.GetTop(), engine.ctx.IsString(-1))
	if engine.ctx.GetTop() != 1 || !engine.ctx.IsString(-1) {
		return duktape.DUK_RET_ERROR
	}
	dev := engine.model.EnsureDevice(engine.ctx.GetString(-1))
	engine.ctx.PushGoObject(dev)
	return 1
}

func (engine *ESEngine) esWbCellObject() int {
	if engine.ctx.GetTop() != 2 || !engine.ctx.IsString(-1) || !engine.ctx.IsObject(-2) {
		return duktape.DUK_RET_ERROR
	}
	dev, ok := engine.ctx.GetGoObject(-2).(CellModelDevice)
	if !ok {
		wbgo.Error.Printf("invalid _wbCellObject call")
		return duktape.DUK_RET_TYPE_ERROR
	}
	cell := dev.EnsureCell(engine.ctx.GetString(-1))
	engine.ctx.PushGoObject(cell)
	engine.ctx.DefineFunctions(map[string]func() int{
		"rawValue": func() int {
			engine.trackCell(cell)
			engine.ctx.PushString(cell.RawValue())
			return 1
		},
		"value": func() int {
			engine.trackCell(cell)
			m := objx.New(map[string]interface{}{
				"v": cell.Value(),
			})
			engine.ctx.PushJSObject(m)
			return 1
		},
		"setValue": func() int {
			engine.trackCell(cell)
			if engine.ctx.GetTop() != 1 || !engine.ctx.IsObject(-1) {
				return duktape.DUK_RET_ERROR
			}
			m, ok := engine.ctx.GetJSObject(-1).(objx.Map)
			if !ok || !m.Has("v") {
				wbgo.Error.Printf("invalid cell definition")
				return duktape.DUK_RET_TYPE_ERROR
			}
			cell.SetValue(m["v"])
			return 1
		},
		"isComplete": func() int {
			engine.trackCell(cell)
			engine.ctx.PushBoolean(cell.IsComplete())
			return 1
		},
	})
	return 1
}

func (engine *ESEngine) esWbStartTimer() int {
	if engine.ctx.GetTop() != 3 || !engine.ctx.IsNumber(1) {
		// FIXME: need to throw proper exception here
		wbgo.Error.Println("bad _wbStartTimer call")
		return duktape.DUK_RET_ERROR
	}

	name := NO_TIMER_NAME
	if engine.ctx.IsString(0) {
		name = engine.ctx.ToString(0)
		if name == "" {
			wbgo.Error.Println("empty timer name")
			return duktape.DUK_RET_ERROR
		}
		engine.stopTimerByName(name)
	} else if !engine.ctx.IsFunction(0) {
		wbgo.Error.Println("invalid timer spec")
		return duktape.DUK_RET_ERROR
	}

	ms := engine.ctx.GetNumber(1)
	periodic := engine.ctx.ToBoolean(2)

	var callback func()
	if name == NO_TIMER_NAME {
		f := engine.ctx.WrapCallback(0)
		callback = func() { f(nil) }
	}

	interval := time.Duration(ms * float64(time.Millisecond))
	engine.ctx.PushNumber(
		float64(engine.startTimer(name, callback, interval, periodic)))
	return 1
}

func (engine *ESEngine) esWbStopTimer() int {
	if engine.ctx.GetTop() != 1 {
		return duktape.DUK_RET_ERROR
	}
	if engine.ctx.IsNumber(0) {
		n := engine.ctx.GetInt(-1)
		if n == 0 {
			wbgo.Error.Printf("timer id cannot be zero")
			return 0
		}
		engine.stopTimerByIndex(n)
	} else if engine.ctx.IsString(0) {
		engine.stopTimerByName(engine.ctx.ToString(0))
	} else {
		return duktape.DUK_RET_ERROR
	}
	return 0
}

func (engine *ESEngine) esWbCheckCurrentTimer() int {
	if engine.ctx.GetTop() != 1 || !engine.ctx.IsString(0) {
		return duktape.DUK_RET_ERROR
	}
	timerName := engine.ctx.ToString(0)
	engine.ctx.PushBoolean(engine.checkTimer(timerName))
	return 1
}

func (engine *ESEngine) esWbSpawn() int {
	if engine.ctx.GetTop() != 5 || !engine.ctx.IsArray(0) || !engine.ctx.IsBoolean(2) ||
		!engine.ctx.IsBoolean(3) {
		return duktape.DUK_RET_ERROR
	}

	args := engine.ctx.StringArrayToGo(0)
	if len(args) == 0 {
		return duktape.DUK_RET_ERROR
	}

	callbackFn := ESCallbackFunc(nil)

	if engine.ctx.IsFunction(1) {
		callbackFn = engine.ctx.WrapCallback(1)
	} else if !engine.ctx.IsNullOrUndefined(1) {
		return duktape.DUK_RET_ERROR
	}

	var input *string
	if engine.ctx.IsString(4) {
		instr := engine.ctx.GetString(4)
		input = &instr
	} else if !engine.ctx.IsNullOrUndefined(4) {
		return duktape.DUK_RET_ERROR
	}

	captureOutput := engine.ctx.GetBoolean(2)
	captureErrorOutput := engine.ctx.GetBoolean(3)
	command := engine.ctx.GetString(0)
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
			wbgo.Error.Printf("command '%s' failed: %s", command, err)
		}
	}()
	return 0
}

func (engine *ESEngine) esWbDefineRule() int {
	if engine.ctx.GetTop() != 2 || !engine.ctx.IsString(0) || !engine.ctx.IsObject(1) {
		engine.logFunc(fmt.Sprintf("bad rule definition"))
		return duktape.DUK_RET_ERROR
	}
	name := engine.ctx.GetString(0)
	if rule, err := engine.buildRule(name, 1); err != nil {
		// FIXME: proper error handling
		engine.logFunc(fmt.Sprintf("bad definition of rule '%s': %s", name, err))
		return duktape.DUK_RET_ERROR
	} else {
		engine.defineRule(rule)
	}
	return 0
}

func (engine *ESEngine) esWbRunRules() int {
	switch engine.ctx.GetTop() {
	case 0:
		engine.RunRules(nil, NO_TIMER_NAME)
	case 2:
		devName := engine.ctx.SafeToString(0)
		cellName := engine.ctx.SafeToString(1)
		engine.RunRules(&CellSpec{devName, cellName}, NO_TIMER_NAME)
	default:
		return duktape.DUK_RET_ERROR
	}
	return 0
}