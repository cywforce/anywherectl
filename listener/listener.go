package listener

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/pefish/anywherectl/internal/protocol"
	"github.com/pefish/anywherectl/internal/version"
	go_logger "github.com/pefish/go-logger"
	"log"
	"net"
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
	isReconnectChan chan <- bool
}

func NewListener(name string) *Listener {
	return &Listener{
		Name:            name,
		sendCommandLock: sync.Mutex{},
		connExit:        make(chan bool, 1),
	}
}

func (s *Listener) DecorateFlagSet(flagSet *flag.FlagSet) {
	flagSet.String("config", "", "path to config file")
	flagSet.String("name", "pefish", "listener name")
	flagSet.String("server-token", "", "server token to connect. max length 32")
	flagSet.String("server-address", "0.0.0.0:8181", "server address to connect")
}

func (s *Listener) ParseFlagSet(flagSet *flag.FlagSet) {
	err := flagSet.Parse(os.Args[2:])
	if err != nil {
		log.Fatal(err)
	}
}

func (l *Listener) Start(finishChan chan<- bool, flagSet *flag.FlagSet) {
	l.finishChan = finishChan

	serverToken := flagSet.Lookup("server-token").Value.(flag.Getter).Get().(string)
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

	serverAddress := flagSet.Lookup("server-address").Value.(flag.Getter).Get().(string)
	l.serverAddress = serverAddress

	l.name = flagSet.Lookup("name").Value.(flag.Getter).Get().(string)
	// 连接服务器
	rm := NewReconnectManager()
	connChan, isReconnectChan := rm.Reconnect(l.serverAddress)
	l.isReconnectChan = isReconnectChan

	go func() {
		for {
			conn := <- connChan
			go_logger.Logger.InfoF("server '%s' connected!! start register...", conn.RemoteAddr())
			l.serverConn = conn

			// 开始接收消息
			err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)) // 设置tcp连接的读超时
			if err != nil {
				go_logger.Logger.WarnF("failed to set conn timeout - %s", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			l.cancelFunc = cancel
			go l.receiveMessageLoop(ctx, conn)

			// 开始注册
			err = l.sendCommandToServer("REGISTER", []string{
				"cvc",
			})
			if err != nil {
				go_logger.Logger.Error("write params to conn err - %s", err)
				l.Exit()
				break
			}
		}
	}()

}

func (l *Listener) receiveMessageLoop(ctx context.Context, conn net.Conn) {
	var zeroTime time.Time
	err := conn.SetDeadline(zeroTime)
	if err != nil {
		go_logger.Logger.WarnF("failed to set conn timeout - %s", err)
	}
	for {
		select {
		case <-ctx.Done():
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
			err = l.execCommand(conn, packageData.Command, packageData.Params)
			if err != nil {
				go_logger.Logger.ErrorF("failed to execCommand command - %s", err)
				goto exit
			}
		}

	}
exit:
	conn.Close()
	l.Exit()
exitMessageLoop:
	conn.Close()
}

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
	} else if name == "ERROR" {
		go_logger.Logger.ErrorF("connection error !!! - %s", params[0])
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
