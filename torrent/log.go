package torrent

import (
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// 初始化zap日志库
var logger *zap.Logger
var logFile *os.File

// InitLogger 根据命令行参数初始化日志配置
// 如果指定了-log参数，日志将输出到文件，否则输出到控制台
func InitLogger(enableFileLogging bool) {
	var err error
	logConfig := zap.NewDevelopmentConfig()
	logConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder

	// 如果启用文件日志
	if enableFileLogging {
		// 创建logs目录（如果不存在）
		os.MkdirAll("logs", 0755)

		// 创建日志文件，使用当前时间作为文件名
		timeStr := time.Now().Format("2006-01-02_15-04-05")
		logFilePath := fmt.Sprintf("logs/bt_download_%s.log", timeStr)
		logFile, err = os.Create(logFilePath)
		if err != nil {
			fmt.Printf("创建日志文件失败: %v，将使用控制台输出\n", err)
		} else {
			// 配置日志输出到文件
			logConfig.OutputPaths = []string{logFilePath}
			logConfig.ErrorOutputPaths = []string{logFilePath}
			fmt.Printf("日志将输出到文件: %s\n", logFilePath)
		}
	}

	// 构建日志器
	logger, err = logConfig.Build()
	if err != nil {
		fmt.Println("初始化日志库失败:", err)
		logger = zap.NewNop()
	}
}

// CloseLogger 关闭日志文件
// 在程序退出前调用，确保日志文件被正确关闭
func CloseLogger() {
	if logFile != nil {
		logger.Sync() // 确保所有日志都被写入
		logFile.Close()
		logFile = nil
	}
}

func init() {
	// 默认初始化为控制台输出
	InitLogger(false)
}
