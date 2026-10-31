package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dushixiang/uart_sms_forwarder/internal"
	"github.com/go-orz/orz"
	_ "github.com/go-orz/orz/drivers/sqlite"
)

const (
	systemdServiceName = "uart_sms_forwarder.service"
	systemdServicePath = "/etc/systemd/system/" + systemdServiceName
)

type commandOptions struct {
	configPath     string
	installService bool
}

func main() {
	options, err := parseCommandOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	configPath, err := prepareSelectedConfigPath(options.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if options.installService {
		absoluteConfigPath, err := filepath.Abs(configPath)
		if err == nil {
			err = installSystemdService(absoluteConfigPath)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "安装 systemd 服务失败:", err)
			os.Exit(1)
		}
		fmt.Printf("systemd 服务已安装并启动: %s\n", systemdServiceName)
		fmt.Printf("查看状态: systemctl status %s\n", systemdServiceName)
		return
	}
	if err := prepareRuntimeDirectories(configPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	internal.Run(configPath)
}

// prepareConfigPath makes relative database and log paths resolve next to the
// selected config file. This is especially important on Windows, where an exe
// may be launched from Explorer, a shortcut, or a service with another working
// directory.
func prepareConfigPath(args []string) (string, error) {
	options, err := parseCommandOptions(args)
	if err != nil {
		return "", err
	}
	return prepareSelectedConfigPath(options.configPath)
}

func parseCommandOptions(args []string) (commandOptions, error) {
	flags := flag.NewFlagSet("uart_sms_forwarder", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configOption := flags.String("config", "", "配置文件路径")
	installService := flags.Bool("install-service", false, "安装并启动 Linux systemd 服务")
	if err := flags.Parse(args); err != nil {
		return commandOptions{}, err
	}
	if flags.NArg() != 0 {
		return commandOptions{}, fmt.Errorf("不支持的位置参数: %s", flags.Arg(0))
	}

	configPath := *configOption
	if configPath == "" {
		configPath = os.Getenv("UART_SMS_FORWARDER_CONFIG")
	}
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	return commandOptions{configPath: configPath, installService: *installService}, nil
}

func prepareSelectedConfigPath(configPath string) (string, error) {
	absolutePath, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("解析配置文件路径失败: %w", err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return "", fmt.Errorf("无法读取配置文件 %q: %w", absolutePath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("配置文件路径指向目录: %q", absolutePath)
	}
	if err := os.Chdir(filepath.Dir(absolutePath)); err != nil {
		return "", fmt.Errorf("切换到配置目录失败: %w", err)
	}
	return filepath.Base(absolutePath), nil
}

func installSystemdService(configPath string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("此命令仅支持 Linux，当前系统为 %s", runtime.GOOS)
	}
	if err := prepareRuntimeDirectories(configPath); err != nil {
		return err
	}
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		return fmt.Errorf("解析程序路径失败: %w", err)
	}
	if resolvedPath, resolveErr := filepath.EvalSymlinks(executablePath); resolveErr == nil {
		executablePath = resolvedPath
	}

	unit, err := buildSystemdUnit(executablePath, configPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(systemdServicePath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败（请使用 sudo 运行）: %w", systemdServicePath, err)
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", systemdServiceName},
		{"restart", systemdServiceName},
	} {
		command := exec.Command("systemctl", args...)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("执行 systemctl %s 失败: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

// prepareRuntimeDirectories uses the framework's own loader so configured
// paths, defaults and environment overrides match the subsequent database open.
// Directory preparation is part of the binary startup, not just start.ps1.
func prepareRuntimeDirectories(configPath string) error {
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	manager := orz.NewConfigManager()
	if err := manager.LoadFromFile(absoluteConfigPath); err != nil {
		return fmt.Errorf("load runtime directory configuration: %w", err)
	}
	config := manager.GetConfig()
	if config == nil {
		return fmt.Errorf("cannot decode runtime directory configuration")
	}
	baseDirectory := filepath.Dir(absoluteConfigPath)
	createParent := func(filename, description string) error {
		if filename == "" {
			return nil
		}
		if !filepath.IsAbs(filename) {
			filename = filepath.Join(baseDirectory, filename)
		}
		directory := filepath.Dir(filename)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return fmt.Errorf("cannot create %s directory %q; check the path and write permissions: %w", description, directory, err)
		}
		return nil
	}
	if config.Database.Enabled && config.Database.Type == orz.DatabaseSqlite {
		dsn := config.Database.URL
		if dsn == "" {
			dsn = config.Database.Sqlite.Path
		}
		filename, err := sqliteDatabaseFilename(dsn)
		if err != nil {
			return fmt.Errorf("invalid SQLite database path: %w", err)
		}
		if err := createParent(filename, "SQLite database"); err != nil {
			return err
		}
	}
	return createParent(config.Log.Filename, "log")
}

// SQLite URI parameters are not filesystem path characters. Memory databases
// and read-only connections must not create directories at startup.
func sqliteDatabaseFilename(dsn string) (string, error) {
	if dsn == "" || strings.HasPrefix(dsn, ":memory:") {
		return "", nil
	}
	if !strings.HasPrefix(dsn, "file:") {
		filename, _, _ := strings.Cut(dsn, "?")
		return filename, nil
	}
	uri, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	mode := uri.Query().Get("mode")
	if mode == "memory" || mode == "ro" {
		return "", nil
	}
	filename := uri.Path
	if uri.Opaque != "" {
		filename, err = url.PathUnescape(uri.Opaque)
		if err != nil {
			return "", err
		}
	}
	if filename == ":memory:" {
		return "", nil
	}
	if uri.Host != "" && uri.Host != "localhost" {
		return "", fmt.Errorf("unsupported SQLite file URI host %q", uri.Host)
	}
	if runtime.GOOS == "windows" && len(filename) >= 3 && filename[0] == '/' && filename[2] == ':' {
		filename = filename[1:]
	}
	return filepath.FromSlash(filename), nil
}

func buildSystemdUnit(executablePath, configPath string) (string, error) {
	if strings.ContainsAny(executablePath, "\r\n") || strings.ContainsAny(configPath, "\r\n") {
		return "", fmt.Errorf("程序或配置文件路径不能包含换行符")
	}
	quote := func(value string) string {
		value = strings.ReplaceAll(value, `\`, `\\`)
		value = strings.ReplaceAll(value, `"`, `\"`)
		value = strings.ReplaceAll(value, `%`, `%%`)
		value = strings.ReplaceAll(value, `$`, `$$`)
		return `"` + value + `"`
	}
	workingDirectory := path.Dir(configPath)
	workingDirectory = strings.ReplaceAll(workingDirectory, `%`, `%%`)
	return fmt.Sprintf(`[Unit]
Description=UART SMS Forwarder
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s -config %s
Restart=always
RestartSec=10
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`, workingDirectory, quote(executablePath), quote(configPath)), nil
}

func defaultConfigPath() string {
	const filename = "config.yaml"
	if _, err := os.Stat(filename); err == nil {
		return filename
	}
	executable, err := os.Executable()
	if err == nil {
		besideExecutable := filepath.Join(filepath.Dir(executable), filename)
		if _, statErr := os.Stat(besideExecutable); statErr == nil {
			return besideExecutable
		}
	}
	return filename
}
