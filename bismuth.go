package bismuth

import (
    "bufio"
    "bytes"
    "errors"
    "fmt"
    "io"
    "net"
    "os"
    "path"
    "strings"
    "sync"
    "golang.org/x/crypto/ssh"
    "golang.org/x/crypto/ssh/agent"
    "github.com/tillberg/ansi-log"
)

const maxSessions = 5

type ExecContext struct {
    mutex      sync.Mutex
    username   string
    hostname   string
    port       int

    sshClient  *ssh.Client
    connected  bool

    numRunning int
    numWaiting int
    poolDone   chan bool

    logger     *log.Logger
    nameAnsi   string

    uname      string
    env        map[string]string
}

var onceInit sync.Once

func (ctx *ExecContext) Init() {
    ctx.poolDone = make(chan bool)
    ctx.port = 22
    ctx.env = make(map[string]string)

    onceInit.Do(func () {
        log.AddAnsiColorCode("host", 33)
        log.AddAnsiColorCode("path", 36)
    })
    ctx.logger = ctx.newLogger("")
    ctx.updatedHostname()

}
func NewExecContext() *ExecContext {
    ctx := &ExecContext{}
    ctx.Init()
    return ctx
}

func (ctx *ExecContext) lock() { ctx.mutex.Lock() }
func (ctx *ExecContext) unlock() { ctx.mutex.Unlock() }

func (ctx *ExecContext) close() {
    if ctx.sshClient != nil {
        ctx.sshClient.Close()
        ctx.sshClient = nil
    }
}

func (ctx *ExecContext) connect() error {
    ctx.lock()
    defer ctx.unlock()
    if ctx.connected {
        return errors.New("Already connected")
    }
    if ctx.hostname != "" {
        agentConn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
        if err != nil { return err }
        defer agentConn.Close()
        ag := agent.NewClient(agentConn)
        auths := []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}
        config := &ssh.ClientConfig{
            User: ctx.username,
            Auth: auths,
        }
        ctx.sshClient, err = ssh.Dial("tcp", fmt.Sprintf("%s:%d", ctx.hostname, ctx.port), config)
        if err != nil { return err }
    }
    ctx.connected = true
    return nil
}

func (ctx *ExecContext) Connect() error {
    err := ctx.connect()
    if err != nil { return err }

    done := make(chan error)
    numTasks := 0
    doTask := func(fn func()) {
        numTasks++
        go fn()
    }
    doTask(func() {
        stdout, err := ctx.Output("uname")
        if err != nil {
            done<-err
        } else {
            ctx.uname = strings.TrimSpace(string(stdout))
            done<-nil
        }
    })
    doTask(func() {
        stdout, err := ctx.Output("env")
        if err != nil {
            done<-err
        } else {
            scanner := bufio.NewScanner(strings.NewReader(string(stdout)))
            for scanner.Scan() {
                line := scanner.Text()
                parts := strings.SplitN(line, "=", 2)
                if len(parts) < 2 {
                    done<-errors.New(fmt.Sprintf("Could not parse env line [%s]", line))
                    return
                }
                ctx.env[parts[0]] = parts[1]
            }
            done<-nil
        }
    })
    for i := 0; i < numTasks; i++ {
        err := <-done
        if err != nil {
            return err
        }
    }
    return nil
}

func (ctx *ExecContext) assertConnected() error {
    if !ctx.connected {
        return errors.New("Not connected. Call Connect first.")
    }
    return nil
}

func (ctx *ExecContext) _makeSession() (Session, error) {
    var session Session
    if ctx.hostname != "" {
        sshSession, err := ctx.sshClient.NewSession()
        if err != nil { return nil, err }
        session = NewSshSession(sshSession)
    } else {
        session = NewLocalSession()
    }
    return session, nil
}

func (ctx *ExecContext) MakeSession() (Session, error) {
    ctx.lock()
    defer ctx.unlock()
    err := ctx.assertConnected()
    if err != nil {
        return nil, err
    }
    if ctx.numRunning < maxSessions {
        ctx.numRunning++
    } else {
        ctx.numWaiting++
        ctx.unlock()
        <-ctx.poolDone
        ctx.lock()
    }
    return ctx._makeSession()
}

func (ctx *ExecContext) closeSession(session Session) {
    session.Close()
    ctx.lock()
    ctx.numRunning--
    if (ctx.numWaiting > 0) {
        ctx.poolDone<-true
        ctx.numWaiting--
    }
    ctx.unlock()
}

func (ctx *ExecContext) Username() string {
    ctx.lock()
    defer ctx.unlock()
    return ctx.username
}

func (ctx *ExecContext) Hostname() string {
    ctx.lock()
    defer ctx.unlock()
    return ctx.hostname
}

func (ctx *ExecContext) SetUsername(s string) {
    ctx.lock()
    defer ctx.unlock()
    ctx.close()
    ctx.username = s
}

func (ctx *ExecContext) SetHostname(s string) {
    ctx.lock()
    defer ctx.unlock()
    ctx.close()
    ctx.hostname = s
    ctx.updatedHostname()
}

func (ctx *ExecContext) updatedHostname() {
    hostname := ctx.hostname
    if hostname == "" { hostname = "localhost" }
    ctx.nameAnsi = ctx.logger.Colorify(fmt.Sprintf("@(host:%s)", hostname))
    ctx.logger.SetPrefix(fmt.Sprintf("@(dim)[%s] ", ctx.nameAnsi))
}

func (ctx *ExecContext) SshAddress() string {
    return fmt.Sprintf("%s@%s", ctx.Username(), ctx.Hostname())
}

func (ctx *ExecContext) NameAnsi() string {
    ctx.lock()
    defer ctx.unlock()
    return ctx.nameAnsi
}

func (ctx *ExecContext) newLogger(suffix string) *log.Logger {
    logger := log.New(os.Stderr, "", 0)
    prefix := fmt.Sprintf("@(dim)[%s] ", ctx.nameAnsi)
    if len(suffix) > 0 {
        prefix = fmt.Sprintf("@(dim)[%s:%s] ", ctx.nameAnsi, suffix)
    }
    logger.EnableColorTemplate()
    logger.SetPrefix(prefix)
    return logger
}

func (ctx *ExecContext) NewLogger(suffix string) *log.Logger {
    ctx.lock()
    defer ctx.unlock()
    return ctx.newLogger(suffix)
}

func (ctx *ExecContext) Logger() *log.Logger {
    ctx.lock()
    defer ctx.unlock()
    return ctx.logger
}

func (ctx *ExecContext) StartCmdAndWait(session Session) (retCode int, err error) {
    cmdlog := ctx.newLogger("")
    cmdlog.Printf("@(dim:$) %s\n", session.GetFullCmdShell())
    cmdlog.Close()
    _, err = session.Start()
    if err != nil { return -1, err }
    defer ctx.closeSession(session)
    return session.Wait()
}

type SessionSetupFn func(session Session, ready chan error, done chan bool)

func (ctx *ExecContext) ExecSession(setupFns ...SessionSetupFn) (int, error) {
    session, err := ctx.MakeSession()
    if err != nil { return -1, err }
    ready := make(chan error)
    done := make(chan bool)
    defer func() { for _, _ = range setupFns { done<-true } }()
    for _, setupFn := range setupFns {
        go setupFn(session, ready, done)
        err = <-ready
        if err != nil {
            return -1, err
        }
    }
    retCode, err := ctx.StartCmdAndWait(session)
    return retCode, err
}

func (ctx *ExecContext) SessionQuote(suffix string) SessionSetupFn {
    fn := func(session Session, ready chan error, done chan bool) {
        stdout := ctx.newLogger(suffix)
        stderr := ctx.newLogger(suffix)
        defer stdout.Close()
        defer stderr.Close()
        session.SetStdout(stdout)
        session.SetStderr(stderr)
        ready<-nil
        <-done
    }
    return fn
}

func SessionShell(cmd string) SessionSetupFn {
    fn := func(session Session, ready chan error, done chan bool) {
        session.SetCmdShell(cmd)
        ready<-nil
        <-done
    }
    return fn
}

func SessionArgs(args ...string) SessionSetupFn {
    fn := func(session Session, ready chan error, done chan bool) {
        session.SetCmdArgs(args...)
        ready<-nil
        <-done
    }
    return fn
}

func SessionCwd(cwd string) SessionSetupFn {
    fn := func(session Session, ready chan error, done chan bool) {
        session.SetCwd(cwd)
        ready<-nil
        <-done
    }
    return fn
}

func SessionBuffer() (SessionSetupFn, chan []byte) {
    bufChan := make(chan []byte)
    fn := func(session Session, ready chan error, done chan bool) {
        var bufOut bytes.Buffer
        var bufErr bytes.Buffer
        session.SetStdout(&bufOut)
        session.SetStderr(&bufErr)
        ready<-nil
        <-done
        bufChan<-bufOut.Bytes()
        bufChan<-bufErr.Bytes()
    }
    return fn, bufChan
}

func SessionPipeStdout(stdout io.Writer) SessionSetupFn {
    return func(session Session, ready chan error, done chan bool) {
        session.SetStdout(stdout)
        ready<-nil
        <-done
    }
}

func SessionPipeStdin(chanStdin chan io.Writer) SessionSetupFn {
    return func(session Session, ready chan error, done chan bool) {
        stdin, err := session.StdinPipe()
        if err != nil {
            ready<-err
            return
        }
        chanStdin<-stdin
        ready<-nil
        <-done
    }
}

func (ctx *ExecContext) QuotePipeOut(suffix string, stdout io.Writer, cwd string, args ...string) (err error) {
    _, err = ctx.ExecSession(SessionPipeStdout(stdout), SessionCwd(cwd), SessionArgs(args...), ctx.SessionQuote(suffix))
    return err
}

func (ctx *ExecContext) QuotePipeIn(suffix string, chanStdin chan io.Writer, cwd string, args ...string) (err error) {
    _, err = ctx.ExecSession(SessionPipeStdin(chanStdin), SessionCwd(cwd), SessionArgs(args...), ctx.SessionQuote(suffix))
    return err
}

func (ctx *ExecContext) QuoteShell(suffix string, s string) (err error) {
    _, err = ctx.ExecSession(SessionShell(s), ctx.SessionQuote(suffix))
    return err
}

func (ctx *ExecContext) QuoteCwd(suffix string, cwd string, args ...string) (err error) {
    _, err = ctx.ExecSession(SessionCwd(cwd), SessionArgs(args...), ctx.SessionQuote(suffix))
    return err
}

func (ctx *ExecContext) Quote(suffix string, args ...string) (err error) {
    _, err = ctx.ExecSession(SessionArgs(args...), ctx.SessionQuote(suffix))
    return err
}

func (ctx *ExecContext) RunShell(s string) (stdout []byte, stderr []byte, retCode int, err error) {
    bufSetup, bufChan := SessionBuffer()
    retCode, err = ctx.ExecSession(bufSetup, SessionShell(s))
    stdout = <-bufChan
    stderr = <-bufChan
    return stdout, stderr, retCode, nil
}

func (ctx *ExecContext) RunCwd(cwd string, args ...string) (stdout []byte, stderr []byte, retCode int, err error) {
    bufSetup, bufChan := SessionBuffer()
    retCode, err = ctx.ExecSession(bufSetup, SessionCwd(cwd), SessionArgs(args...))
    stdout = <-bufChan
    stderr = <-bufChan
    return stdout, stderr, retCode, nil
}

func (ctx *ExecContext) Run(args ...string) (stdout []byte, stderr []byte, retCode int, err error) {
    bufSetup, bufChan := SessionBuffer()
    retCode, err = ctx.ExecSession(bufSetup, SessionArgs(args...))
    stdout = <-bufChan
    stderr = <-bufChan
    return stdout, stderr, retCode, nil
}

func (ctx *ExecContext) OutputShell(s string) (stdout string, err error) {
    bufSetup, bufChan := SessionBuffer()
    _, err = ctx.ExecSession(bufSetup, SessionShell(s))
    stdout = strings.TrimSpace(string(<-bufChan))
    <-bufChan // ignore stderr
    return stdout, err
}

func (ctx *ExecContext) OutputCwd(cwd string, args ...string) (stdout string, err error) {
    bufSetup, bufChan := SessionBuffer()
    _, err = ctx.ExecSession(bufSetup, SessionCwd(cwd), SessionArgs(args...))
    stdout = strings.TrimSpace(string(<-bufChan))
    <-bufChan // ignore stderr
    return stdout, err
}

func (ctx *ExecContext) Output(args ...string) (stdout string, err error) {
    bufSetup, bufChan := SessionBuffer()
    _, err = ctx.ExecSession(bufSetup, SessionArgs(args...))
    stdout = strings.TrimSpace(string(<-bufChan))
    <-bufChan // ignore stderr
    return stdout, err
}

// This is super-duper slow because SCP is super-duper slow. But it works, at least some of the time.
func (ctx *ExecContext) UploadRecursive(srcPath string, destContext *ExecContext, destPath string) error {
    status := ctx.NewLogger("")
    status.Printf("Uploading %s to dest %s\n", srcPath, destPath)
    srcSession, err := ctx.MakeSession()
    if err != nil { return err }
    destSession, err := destContext.MakeSession()
    if err != nil { return err }

    srcStderr := ctx.NewLogger("scp-src")
    defer srcStderr.Close()
    srcSession.SetStderr(srcStderr)

    destStderr := destContext.NewLogger("scp-dest")
    defer destStderr.Close()
    destSession.SetStderr(destStderr)

    srcStdin, err := srcSession.StdinPipe()
    destStdin, err := destSession.StdinPipe()

    // Uncomment to watch:
    // srcStdoutLog := ctx.NewLogger("scp-src/out")
    // defer srcStdoutLog.Close()
    // destStdoutLog := ctx.NewLogger("scp-dest/out")
    // defer destStdoutLog.Close()
    // srcSession.SetStdout(io.MultiWriter(destStdin, srcStdoutLog))
    // destSession.SetStdout(io.MultiWriter(srcStdin, destStdoutLog))

    srcSession.SetStdout(destStdin)
    destSession.SetStdout(srcStdin)

    done := make(chan error)
    go func() {
        status.Printf("Starting source scp %s\n", ctx.AbsPath(srcPath))
        srcSession.SetCmdArgs("scp", "-rfp", ctx.AbsPath(srcPath))
        retCode, err := ctx.StartCmdAndWait(srcSession)
        status.Printf("Src scp exited with %d and %#v\n", retCode, err)
        done<-err
    }()
    go func() {
        status.Printf("Starting dest scp %s\n", destContext.AbsPath(destPath))
        destSession.SetCmdArgs("scp", "-tr", destContext.AbsPath(destPath))
        retCode, err := destContext.StartCmdAndWait(destSession)
        status.Printf("Dest scp exited with %d and %#v\n", retCode, err)
        done<-err
    }()
    err = <-done
    if err != nil { return err }
    err = <-done
    if err != nil { return err }
    return err
}

func (ctx *ExecContext) AbsPath(p string) string {
    // Rewrite home-relative paths as simply relative paths, which
    // we resolve in the next step relative to $HOME
    if p == "~" { p = "" }
    if len(p) >= 2 && p[:2] == "~/" {
        p = p[2:]
    }
    if !path.IsAbs(p) {
        p = path.Join([]string{ctx.env["HOME"], p}...)
    }
    return path.Clean(p)
}

func (ctx *ExecContext) Stat(p string) (os.FileInfo, error) {
    flagStr := "-c"
    formatStr := "%F,%f,%s,%Y"
    if ctx.IsDarwin() {
        flagStr = "-f"
        formatStr = "%HT,%Xp,%z,%m"
    }
    p = ctx.AbsPath(p)
    stdout, _, retCode, err := ctx.Run("stat", flagStr, formatStr, p)
    // log.Printf("stat %s -- %s\n", p, strings.TrimSpace(string(stdout)))
    if err != nil { return nil, err }
    if retCode == 1 { return nil, nil }
    if retCode != 0 { return nil, errors.New(fmt.Sprintf("stat returned unexpected code %d", retCode)) }
    fileInfo, err := NewFileInfoIsh(p, string(stdout))
    if err != nil { return nil, err }
    return fileInfo, nil
}

func (ctx *ExecContext) PathExists(path string) (bool, error) {
    stat, err := ctx.Stat(path)
    if err != nil { return false, err }
    return stat != nil, nil
}

func (ctx *ExecContext) Close() {
    ctx.mutex.Lock()
    defer ctx.mutex.Unlock()
    ctx.close()
}

func (ctx *ExecContext) Uname() string {
    ctx.mutex.Lock()
    defer ctx.mutex.Unlock()
    err := ctx.assertConnected()
    if err != nil {
        panic(err)
    }
    return ctx.uname
}

func (ctx *ExecContext) IsLocal() bool {
    return ctx.Hostname() == ""
}

func (ctx *ExecContext) IsWindows() bool {
    return ctx.Uname() == "Windows" // XXX this won't actually work
}

func (ctx *ExecContext) IsDarwin() bool {
    return ctx.Uname() == "Darwin"
}

func (ctx *ExecContext) IsLinux() bool {
    return ctx.Uname() == "Linux"
}
