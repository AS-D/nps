package main

import (
	"ehang.io/nps/lib/crypt"
	"ehang.io/nps/lib/file"
	"ehang.io/nps/lib/install"
	"ehang.io/nps/lib/version"
	"ehang.io/nps/server"
	"ehang.io/nps/server/connection"
	"ehang.io/nps/server/tool"
	"ehang.io/nps/web/routers"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"ehang.io/nps/lib/common"
	"ehang.io/nps/lib/daemon"
	"github.com/astaxie/beego"
	"github.com/astaxie/beego/logs"

	"github.com/kardianos/service"
)

var (
	level string
)

const systemdScript = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
{{range $i, $dep := .Dependencies}} 
{{$dep}} {{end}}
[Service]
LimitNOFILE=65536
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
{{if .ChRoot}}RootDirectory={{.ChRoot|cmd}}{{end}}
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .UserName}}User={{.UserName}}{{end}}
{{if .ReloadSignal}}ExecReload=/bin/kill -{{.ReloadSignal}} "$MAINPID"{{end}}
{{if .PIDFile}}PIDFile={{.PIDFile|cmd}}{{end}}
{{if and .LogOutput .HasOutputFileSupport -}}
StandardOutput=file:/var/log/{{.Name}}.out
StandardError=file:/var/log/{{.Name}}.err
{{- end}}
Restart=always
RestartSec=120
[Install]
WantedBy=multi-user.target
`

func main() {
	flag.Parse()
	// init log
	if err := beego.LoadAppConfig("ini", filepath.Join(common.GetRunPath(), "conf", "nps.conf")); err != nil {
		log.Fatalln("load config file error", err.Error())
	}
	common.InitPProfFromFile()
	if level = beego.AppConfig.String("log_level"); level == "" {
		level = "7"
	}
	logs.Reset()
	logs.EnableFuncCallDepth(true)
	logs.SetLogFuncCallDepth(3)
	logPath := beego.AppConfig.String("log_path")
	if logPath == "" {
		logPath = common.GetLogPath()
	}
	if common.IsWindows() {
		logPath = strings.Replace(logPath, "\\", "\\\\", -1)
	}
	// init service
	options := make(service.KeyValue)
	svcConfig := &service.Config{
		Name:        "Nps",
		DisplayName: "nps内网穿透代理服务器",
		Description: "一款轻量级、功能强大的内网穿透代理服务器。支持tcp、udp流量转发，支持内网http代理、内网socks5代理，同时支持snappy压缩、站点保护、加密传输、多路复用、header修改等。支持web图形化管理，集成多用户模式。",
		Option:      options,
	}
	svcConfig.Arguments = append(svcConfig.Arguments, "service")
	if len(os.Args) > 1 && os.Args[1] == "service" {
		_ = logs.SetLogger(logs.AdapterFile, `{"level":`+level+`,"filename":"`+logPath+`","daily":false,"maxlines":100000,"color":true}`)
	} else {
		_ = logs.SetLogger(logs.AdapterConsole, `{"level":`+level+`,"color":true}`)
	}
	if !common.IsWindows() {
		svcConfig.Dependencies = []string{
			"Requires=network.target",
			"After=network-online.target syslog.target"}
		svcConfig.Option["SystemdScript"] = systemdScript
	}
	prg := &nps{}
	prg.exit = make(chan struct{})
	s, err := service.New(prg, svcConfig)
	if err != nil {
		logs.Error(err, "service function disabled")
		run()
		// run without service
		wg := sync.WaitGroup{}
		wg.Add(1)
		wg.Wait()
		return
	}
	if len(os.Args) > 1 && os.Args[1] != "service" {
		switch os.Args[1] {
		case "reload":
			daemon.InitDaemon("nps", common.GetRunPath(), common.GetTmpPath())
			return
		case "install":
			// uninstall before
			_ = service.Control(s, "stop")
			_ = service.Control(s, "uninstall")

			binPath := install.InstallNps()
			svcConfig.Executable = binPath
			s, err := service.New(prg, svcConfig)
			if err != nil {
				logs.Error(err)
				return
			}
			err = service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			return
		case "start", "restart", "stop", "uninstall":
			err := service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			return
		case "update":
			install.UpdateNps()
			return
		default:
			logs.Error("command is not support")
			return
		}
	}
	_ = s.Run()
}

type nps struct {
	exit chan struct{}
}

func (p *nps) Start(s service.Service) error {
	_, _ = s.Status()
	_ = p.run()
	return nil
}
func (p *nps) Stop(s service.Service) error {
	_, _ = s.Status()
	close(p.exit)
	if service.Interactive() {
		os.Exit(0)
	}
	return nil
}

func (p *nps) run() error {
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			logs.Warning("nps: panic serving %v: %v\n%s", err, string(buf))
		}
	}()
	run()
	select {
	case <-p.exit:
		logs.Warning("stop...")
	}
	return nil
}

func run() {
	routers.Init()
	task := &file.Tunnel{
		Mode: "webServer",
	}
	bridgePort, err := beego.AppConfig.Int("bridge_port")
	if err != nil {
		logs.Error("Getting bridge_port error", err)
		os.Exit(0)
	}
	logs.Info("the version of server is %s ,allow client core version to be %s", version.VERSION, version.GetVersion())
	connection.InitConnectionService()
	crypt.InitTls(filepath.Join(common.GetRunPath(), "conf", "server.pem"), filepath.Join(common.GetRunPath(), "conf", "server.key"))
	tool.InitAllowPort()
	tool.StartSystemInfo()
	go server.StartNewServer(bridgePort, task, beego.AppConfig.String("bridge_type"))
}
