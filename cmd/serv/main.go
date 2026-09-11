package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dushixiang/uart_sms_forwarder/internal"
	_ "github.com/go-orz/orz/drivers/sqlite"
)

func main() {
	configPath, err := prepareConfigPath(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	internal.Run(configPath)
}

// prepareConfigPath makes relative database and log paths resolve next to the
// selected config file. This is especially important on Windows, where an exe
// may be launched from Explorer, a shortcut, or a service with another working
// directory.
func prepareConfigPath(args []string) (string, error) {
	flags := flag.NewFlagSet("uart_sms_forwarder", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configOption := flags.String("config", "", "配置文件路径")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("不支持的位置参数: %s", flags.Arg(0))
	}

	configPath := *configOption
	if configPath == "" {
		configPath = os.Getenv("UART_SMS_FORWARDER_CONFIG")
	}
	if configPath == "" {
		configPath = defaultConfigPath()
	}
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
