package main

import (
	wbrules "./wbrules"
	"flag"
	"github.com/contactless/wbgo"
	"os"
	"os/signal"
	"runtime/pprof"
	"time"
)

const DRIVER_CLIENT_ID = "rules"

func main() {
	brokerAddress := flag.String("broker", "tcp://localhost:1883", "MQTT broker url")
	editDir := flag.String("editdir", "", "Editable script directory")
	debug := flag.Bool("debug", false, "Enable debugging")
	useSyslog := flag.Bool("syslog", false, "Use syslog for logging")
	mqttDebug := flag.Bool("mqttdebug", false, "Enable MQTT debugging")
	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to file")
	flag.Parse()
	if flag.NArg() < 1 {
		wbgo.Error.Fatal("must specify rule file/directory name(s)")
	}
	if *useSyslog {
		wbgo.UseSyslog()
	}
	if *debug {
		wbgo.SetDebuggingEnabled(true)
	}
	if *mqttDebug {
		wbgo.EnableMQTTDebugLog()
	}
	model := wbrules.NewCellModel()
	mqttClient := wbgo.NewPahoMQTTClient(*brokerAddress, DRIVER_CLIENT_ID, true)
	driver := wbgo.NewDriver(model, mqttClient)
	driver.SetAutoPoll(false)
	driver.SetAcceptsExternalDevices(true)
	engine := wbrules.NewESEngine(model, mqttClient)
	gotSome := false
	watcher := wbgo.NewDirWatcher("\\.js$", engine)
	if *editDir != "" {
		engine.SetSourceRoot(*editDir)
	}
	for _, path := range flag.Args() {
		if err := watcher.Load(path); err != nil {
			wbgo.Error.Printf("error loading script file/dir %s: %s", path, err)
		} else {
			gotSome = true
		}
	}
	if !gotSome {
		wbgo.Error.Fatalf("no valid scripts found")
	}
	if err := driver.Start(); err != nil {
		wbgo.Error.Fatalf("error starting the driver: %s", err)
	}

	if *editDir != "" {
		rpc := wbgo.NewMQTTRPCServer("wbrules", mqttClient)
		rpc.Register(wbrules.NewEditor(engine))
		rpc.Start()
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			wbgo.Error.Fatalf("error creating profiling file: %s", err)
		}
		pprof.StartCPUProfile(f)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		go func() {
			<-ch
			pprof.StopCPUProfile()
			os.Exit(130)
		}()
	}

	engine.Start()
	for {
		time.Sleep(1 * time.Second)
	}
}
