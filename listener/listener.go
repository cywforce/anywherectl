package listener

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/pefish/anywherectl/internal/protocol"
	"github.com/pefish/anywherectl/internal/version"
	"github.com/pefish/anywherectl/listener/shell"
	"github.com/pefish/go-config"
	go_logger "github.com/pefish/go-logger"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"sync"
	"time"
)

type Listener struct {
	Name            string
	serverToken     string
	serverAddress   string
	sendCommandLock sync.Mutex
	serverConn      net.Conn
	connExit        chan bool
	cancelFunc      context.CancelFunc
	finishChan      chan<- bool
	name            string
	isReconnectChan chan<- bool
	ClientTokens    map[string]interface{}
	clientActions   sync.Map // 保存client的所有action。map[string]ActionData
}

type ActionData struct {
	exitActionChan chan bool
}

func NewListener(name string) *Listener {
	return &Listener{
		Name:     name,
		connExit: make(chan bool, 1),
	}
}

func (s *Listener) DecorateFlagSet(flagSet *flag.FlagSet) {
	flagSet.String("name", "pefish", "listener name")
	flagSet.String("server-token", "", "server token to connect. max length 32")
	flagSet.String("server-address", "0.0.0.0:8181", "server address to connect")
	flagSet.Bool("enable-pprof", false, "enable pprof")
	flagSet.String("pprof-address", "0.0.0.0:9191", "<addr>:<port> to listen on for pprof")
}

func (s *Listener) ParseFlagSet(flagSet *flag.FlagSet) {
	err := flagSet.Parse(os.Args[2:])
	if err != nil {
		log.Fatal(err)
	}
}

func (l *Listener) Start(finishChan chan<- bool, flagSet *flag.FlagSet) {
	l.finishChan = finishChan
	ctx, cancel := context.WithCancel(context.Background())
	l.cancelFunc = cancel

	go_logger.Logger.DebugF("configs: %#v", go_config.Config.GetAll())

	serverToken, err := go_config.Config.GetString("server-token")
	if err != nil {
		go_logger.Logger.ErrorF("get config error - %s", err)
		l.Exit()
		return
	}
	if serverToken == "" {
		go_logger.Logger.Error("server token must be set")
		l.Exit()
		return
	}
	if len(serverToken) > 32 {
		go_logger.Logger.Error("server token too long")
		l.Exit()
		return
	}
	l.serverToken = serverToken

	serverAddress, err := go_config.Config.GetString("server-address")
	if err != nil {
		go_logger.Logger.ErrorF("get config error - %s", err)
		l.Exit()
		return
	}
	l.serverAddress = serverAddress

	l.name, err = go_config.Config.GetString("name")
	if err != nil {
		go_logger.Logger.ErrorF("get config error - %s", err)
		l.Exit()
		return
	}

	l.ClientTokens, err = go_config.Config.GetMapDefault("auth", make(map[string]interface{}))
	if err != nil {
		go_logger.Logger.ErrorF("get config error - %s", err)
		l.Exit()
		return
	}

	// 连接服务器
	rm := NewReconnectManager()
	connChan, isReconnectChan := rm.Reconnect(l.serverAddress)
	l.isReconnectChan = isReconnectChan

	go func() {
		for {
			conn := <-connChan
			go_logger.Logger.InfoF("server '%s' connected!! start register...", conn.RemoteAddr())
			l.serverConn = conn

			// 开始接收消息
			go l.receiveMessageLoop(ctx, conn)

			// 开始注册

			tokensDataStr, err := json.Marshal(l.ClientTokens)
			if err != nil {
				go_logger.Logger.Error("json.Marshal ClientTokens err - %s", err)
				l.Exit()
				break
			}
			err = l.sendCommandToServer("REGISTER", []string{
				string(tokensDataStr),
			})
			if err != nil {
				go_logger.Logger.Error("send command REGISTER err - %s", err)
				l.Exit()
				break
			}
		}
	}()

	pprofEnable, err := go_config.Config.GetBool("enable-pprof")
	if err != nil {
		go_logger.Logger.ErrorF("get config error - %s", err)
		l.Exit()
		return
	}
	pprofAddress, err := go_config.Config.GetString("pprof-address")
	if err != nil {
		go_logger.Logger.ErrorF("get config error - %s", err)
		l.Exit()
		return
	}
	if pprofEnable {
		go func() {
			go_logger.Logger.InfoF("starting pprof server on %s", pprofAddress)
			err := http.ListenAndServe(pprofAddress, nil)
			if err != nil {
				go_logger.Logger.WarnF("pprof server start error - %s", err)
			}
		}()
	}
}

func (l *Listener) receiveMessageLoop(ctx context.Context, conn net.Conn) {
	var zeroTime time.Time
	err := conn.SetDeadline(zeroTime)
	if err != nil {
		go_logger.Logger.WarnF("failed to set conn timeout - %s", err)
	}
	isExitLoop := make(chan bool, 1)
	for {
		select {
		case <-ctx.Done():
			goto exit
		case <- isExitLoop:
			goto exit
		default:
			packageData, err := protocol.ReadPackage(conn)
			if err != nil {
				go_logger.Logger.DebugF("read command and params error - '%s'", err)
				if strings.Contains(err.Error(), "EOF") {
					go_logger.Logger.Error("connection disconnected!! start reconnecting...")
					l.isReconnectChan <- true
					goto exitMessageLoop
				}
				goto exit
			}
			go_logger.Logger.DebugF("received package '%#v'", packageData)

			go func() {
				err = l.execCommand(conn, packageData.Command, packageData.Params)
				if err != nil {
					go_logger.Logger.ErrorF("exec [%s] command err - %s", packageData.Command, err)
					close(isExitLoop)
				}
			}()
		}

	}
exit:
	conn.Close()
	l.Exit()
exitMessageLoop:
	conn.Close()
}

// if return error, conn between listener and server will be closed.
func (l *Listener) execCommand(conn net.Conn, name string, params []string) error {
	if name == "PING" {
		err := l.sendCommandToServer("PONG", nil)
		if err != nil {
			go_logger.Logger.WarnF("failed to exec pong command - %s", err)
		}
	} else if name == "REGISTER_OK" {
		go_logger.Logger.Info("received REGISTER_OK.")
	} else if name == "REGISTER_FAIL" {
		return fmt.Errorf("register error - %s", params[0])
	} else if name == "TOKEN_ERROR" {
		return errors.New("token error")
	} else if name == "VERSION_ERROR" {
		return errors.New("version error")
	} else if name == "CLIENT_CLOSED" {
		clientId := params[0]
		re, ok := l.clientActions.Load(clientId)
		if ok {
			actionData := re.(*ActionData)
			close(actionData.exitActionChan)
			l.clientActions.Delete(clientId)
		}
	} else if name == "SHELL" {
		clientId := params[0]
		go_logger.Logger.InfoF("exec shell command [%s]", params[1])
		cmd, err := shell.GetCmd(params[1])
		if err != nil {
			go_logger.Logger.WarnF("failed to get cmd instance - %s", err)
			return nil
		}
		reader_, err := cmd.StdoutPipe()
		if err != nil {
			go_logger.Logger.WarnF("failed to StdoutPipe - %s", err)
			return nil
		}
		err = cmd.Start()
		if err != nil {
			go_logger.Logger.WarnF("failed to cmd.Start - %s", err)
			return nil
		}
		actionData := &ActionData{
			exitActionChan: make(chan bool, 1),
		}
		l.clientActions.Store(clientId, actionData)
		reader := bufio.NewReader(reader_)

		// 启动定时器监控ReadLine情况，30s没数据强制关闭进程
		timer := time.NewTimer(0)
		go func() {
			for {
				select {
				case <- timer.C: // 持续没有新数据，会触发这里过期
					go_logger.Logger.Warn("ReadLine timeout, kill cmd process")
					cmd.Process.Kill()
				}
				timer.Stop()
				break
			}
		}()
		for {
			timer.Reset(30 * time.Second)

			line, _, err := reader.ReadLine()
			if err != nil {
				if err == io.EOF {
					go_logger.Logger.DebugF("end to ReadLine - %s", err)
					l.sendCommandToServer("SHELL_RESULT", []string{params[0], "2", string(line)})
					return nil
				}
				go_logger.Logger.WarnF("error to ReadLine - %s", err)
				return nil
			}
			l.sendCommandToServer("SHELL_RESULT", []string{params[0], "1", string(line)})
		}
	} else {
		return errors.New("command error")
	}
	return nil
}

func (l *Listener) sendCommandToServer(command string, params []string) error {
	l.sendCommandLock.Lock()
	defer l.sendCommandLock.Unlock()

	_, err := protocol.WritePackage(l.serverConn, &protocol.ProtocolPackage{
		Version:       version.ProtocolVersion,
		ServerToken:   l.serverToken,
		ListenerName:  l.name,
		ListenerToken: "",
		Command:       command,
		Params:        params,
	})
	return err
}

func (s *Listener) Exit() {
	close(s.finishChan)
}

func (s *Listener) Clear() {
	s.cancelFunc()
}
