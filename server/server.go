package chserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	chshare "github.com/jpillora/chisel/share"
	"github.com/jpillora/chisel/share/ccrypto"
	"github.com/jpillora/chisel/share/cio"
	"github.com/jpillora/chisel/share/cnet"
	"github.com/jpillora/chisel/share/settings"
	"github.com/jpillora/requestlog"
	"golang.org/x/crypto/ssh"
)

// Config is the configuration for the chisel service
type Config struct {
	KeySeed                      string
	AuthFile                     string
	AuthURL                      string // eg https://da.server.org/path/to/post
	AuthURLCaCert                string // eg. path to cacert
	AuthURLAssumeUniqueUsernames bool   // assume unique usernames in AuthFile when checking acl
	Auth                         string
	Proxy                        string
	Socks5                       bool
	Reverse                      bool
	KeepAlive                    time.Duration
	TLS                          TLSConfig
}

// Server respresent a chisel service
type Server struct {
	*cio.Logger
	config        *Config
	fingerprint   string
	httpServer    *cnet.HTTPServer
	reverseProxy  *httputil.ReverseProxy
	sessCount     int32
	sessions      *settings.Users
	sshConfig     *ssh.ServerConfig
	users         *settings.UserIndex
	authURL       string
	authURLClient *http.Client
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  settings.EnvInt("WS_BUFF_SIZE", 0),
	WriteBufferSize: settings.EnvInt("WS_BUFF_SIZE", 0),
}

// NewServer creates and returns a new chisel server
func NewServer(c *Config) (*Server, error) {
	server := &Server{
		config:     c,
		httpServer: cnet.NewHTTPServer(),
		Logger:     cio.NewLogger("server"),
		sessions:   settings.NewUsers(),
	}
	server.Info = true
	server.users = settings.NewUserIndex(server.Logger)
	if c.AuthFile != "" {
		if err := server.users.LoadUsers(c.AuthFile); err != nil {
			return nil, err
		}
	}
	if c.Auth != "" {
		u := &settings.User{Addrs: []*regexp.Regexp{settings.UserAllowAll}}
		u.Name, u.Pass = settings.ParseAuth(c.Auth)
		if u.Name != "" {
			server.users.AddUser(u)
		}
	}
	//generate private key (optionally using seed)
	key, err := ccrypto.GenerateKey(c.KeySeed)
	if err != nil {
		log.Fatal("Failed to generate key")
	}
	//convert into ssh.PrivateKey
	private, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatal("Failed to parse key")
	}
	//fingerprint this key
	server.fingerprint = ccrypto.FingerprintKey(private.PublicKey())
	//create ssh config
	server.sshConfig = &ssh.ServerConfig{
		ServerVersion:    "SSH-" + chshare.ProtocolVersion + "-server",
		PasswordCallback: server.authUser,
	}
	server.sshConfig.AddHostKey(private)

	if c.AuthURL != "" {

		server.authURLClient = &http.Client{}
		server.authURL = c.AuthURL

		rootCAs, _ := x509.SystemCertPool()
		if rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}

		if c.AuthURLCaCert != "" {
			certs, err := ioutil.ReadFile(c.AuthURLCaCert)
			if err != nil {
				log.Fatalf("Failed to append %q to RootCAs: %v", c.AuthURLCaCert, err)
			}

			// Append our cert to the system pool
			if ok := rootCAs.AppendCertsFromPEM(certs); !ok {
				log.Println("No certs appended, using system certs only")
			}
		}
		tlsConfig := &tls.Config{
			RootCAs: rootCAs,
		}
		tr := &http.Transport{TLSClientConfig: tlsConfig}
		server.authURLClient.Transport = tr
		server.sshConfig.PasswordCallback = server.authUserURL
		server.config = c
		//fmt.Println("hello")
	}
	//setup reverse proxy
	if c.Proxy != "" {
		u, err := url.Parse(c.Proxy)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, server.Errorf("Missing protocol (%s)", u)
		}
		server.reverseProxy = httputil.NewSingleHostReverseProxy(u)
		//always use proxy host
		server.reverseProxy.Director = func(r *http.Request) {
			//enforce origin, keep path
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
		}
	}
	//print when reverse tunnelling is enabled
	if c.Reverse {
		server.Infof("Reverse tunnelling enabled")
	}
	return server, nil
}

// Run is responsible for starting the chisel service.
// Internally this calls Start then Wait.
func (s *Server) Run(host, port string) error {
	if err := s.Start(host, port); err != nil {
		return err
	}
	return s.Wait()
}

// Start is responsible for kicking off the http server
func (s *Server) Start(host, port string) error {
	return s.StartContext(context.Background(), host, port)
}

// StartContext is responsible for kicking off the http server,
// and can be closed by cancelling the provided context
func (s *Server) StartContext(ctx context.Context, host, port string) error {
	s.Infof("Fingerprint %s", s.fingerprint)
	if s.users.Len() > 0 {
		s.Infof("User authenication enabled")
	}
	if s.reverseProxy != nil {
		s.Infof("Reverse proxy enabled")
	}
	l, err := s.listener(host, port)
	if err != nil {
		return err
	}
	h := http.Handler(http.HandlerFunc(s.handleClientHandler))
	if s.Debug {
		o := requestlog.DefaultOptions
		o.TrustProxy = true
		h = requestlog.WrapWith(h, o)
	}
	return s.httpServer.GoServe(ctx, l, h)
}

// Wait waits for the http server to close
func (s *Server) Wait() error {
	return s.httpServer.Wait()
}

// Close forcibly closes the http server
func (s *Server) Close() error {
	return s.httpServer.Close()
}

// GetFingerprint is used to access the server fingerprint
func (s *Server) GetFingerprint() string {
	return s.fingerprint
}

// authUser is responsible for validating the ssh user / password combination
func (s *Server) authUser(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	// check if user authenication is enable and it not allow all
	if s.users.Len() == 0 {
		return nil, nil
	}
	// check the user exists and has matching password
	n := c.User()
	user, found := s.users.Get(n)
	if !found || user.Pass != string(password) {
		s.Debugf("Login failed for user: %s", n)
		return nil, errors.New("Invalid authentication for username: %s")
	}
	// insert the user session map
	// TODO this should probably have a lock on it given the map isn't thread-safe
	s.sessions.Set(string(c.SessionID()), user)
	return nil, nil
}

// AddUser adds a new user into the server user index
func (s *Server) AddUser(user, pass string, addrs ...string) error {
	authorizedAddrs := []*regexp.Regexp{}
	for _, addr := range addrs {
		authorizedAddr, err := regexp.Compile(addr)
		if err != nil {
			return err
		}
		authorizedAddrs = append(authorizedAddrs, authorizedAddr)
	}
	s.users.AddUser(&settings.User{
		Name:  user,
		Pass:  pass,
		Addrs: authorizedAddrs,
	})
	return nil
}

// DeleteUser removes a user from the server user index
func (s *Server) DeleteUser(user string) {
	s.users.Del(user)
}

// ResetUsers in the server user index.
// Use nil to remove all.
func (s *Server) ResetUsers(users []*settings.User) {
	s.users.Reset(users)
}

//

// authUserURL is authenticating users using a token service
type authUserData struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) authUserURL(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {

	authData := authUserData{
		Username: c.User(),
		Password: string(password),
	}

	authDataJSON, err := json.Marshal(authData)

	if err != nil {
		s.Debugf(err.Error())
		return nil, errors.New("Invalid authentication for username: %s")
	}

	resp, err := s.authURLClient.Post(s.authURL, "application/json", bytes.NewBuffer(authDataJSON))

	if err != nil {
		s.Debugf(err.Error())
		return nil, errors.New("Invalid authentication for username: %s")
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var v map[string]interface{}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			s.Debugf(err.Error())
			return nil, errors.New("Invalid authentication for username: %s")
		}

		err = json.Unmarshal(body, &v)
		if err != nil {
			s.Debugf(err.Error())
			return nil, errors.New("Invalid authentication for username: %s")
		}

		userNameACL := authData.Username
		if s.config.AuthURLAssumeUniqueUsernames {
			userNameParts := strings.Split(authData.Username, "@")
			userNameACL = userNameParts[0]
		}

		userACL, found := s.users.Get(userNameACL)
		if !found {
			s.Infof("Username %s not found, attempting reload of index", userNameACL)
			_ = s.users.LoadUserIndex()
			userACL, found = s.users.Get(userNameACL)
			if !found {
				s.Debugf("Username %s not found", userNameACL)
				return nil, errors.New("Invalid authentication for username: %s")
			}
		}
		fmt.Printf("%+v\n", userACL)

		user := &chshare.User{
			Name:  authData.Username,
			Pass:  authData.Password,
			Addrs: userACL.Addrs,
		}
		s.sessions.Set(string(c.SessionID()), user)
		return nil, nil
	}
	return nil, errors.New("Invalid authentication for username: %s")
}
