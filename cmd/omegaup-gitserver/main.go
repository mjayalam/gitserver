package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/inconshreveable/log15"
	git "github.com/lhchavez/git2go"
	"github.com/omegaup/githttp"
	"github.com/omegaup/gitserver"
	"github.com/omegaup/gitserver/request"
	base "github.com/omegaup/go-base"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"time"
)

var (
	rootPath           = flag.String("root", "", "Root path of all repositories")
	port               = flag.Int("port", 33861, "Port in which the server will listen")
	pprofPort          = flag.Int("pprof-port", 33862, "Port in which the pprof server will listen")
	libinteractivePath = flag.String("libinteractive-path", "/usr/share/java/libinteractive.jar", "Path of libinteractive.jar")
	log                log15.Logger
)

func authorize(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	operation githttp.GitOperation,
) (githttp.AuthorizationLevel, string) {
	username, _, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", "Basic realm=\"Git\"")
		w.WriteHeader(http.StatusUnauthorized)
		return githttp.AuthorizationDenied, ""
	}

	log.Info(
		"Auth",
		"username", username,
		"repository", repositoryName,
		"operation", operation,
	)
	requestContext := request.FromContext(ctx)
	requestContext.CanView = true
	if username == "admin" {
		requestContext.IsAdmin = true
		requestContext.CanEdit = true
		return githttp.AuthorizationAllowed, username
	}
	return githttp.AuthorizationAllowedRestricted, username
}

func referenceDiscovery(
	ctx context.Context,
	repository *git.Repository,
	referenceName string,
) bool {
	requestContext := request.FromContext(ctx)
	if requestContext.CanEdit {
		return true
	}
	if requestContext.HasSolved {
		return referenceName == "refs/heads/public" ||
			referenceName == "refs/heads/protected" ||
			referenceName == "refs/heads/private"
	}

	return referenceName == "refs/heads/public"
}

type muxGitHandler struct {
	gitHandler http.Handler
	zipHandler http.Handler
}

func muxHandler(
	rootPath string,
	protocol *githttp.GitProtocol,
	log log15.Logger,
) http.Handler {
	return &muxGitHandler{
		gitHandler: gitserver.GitHandler(rootPath, protocol, log),
		zipHandler: gitserver.ZipHandler(rootPath, protocol, log),
	}
}

func (h *muxGitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	splitPath := strings.SplitN(r.URL.Path[1:], "/", 2)
	if len(splitPath) == 2 && splitPath[1] == "git-upload-zip" {
		h.zipHandler.ServeHTTP(w, r)
	} else {
		h.gitHandler.ServeHTTP(w, r)
	}
}

func main() {
	flag.Parse()
	log = base.StderrLog()

	stopChan := make(chan os.Signal)
	signal.Notify(stopChan, os.Interrupt)

	protocol := gitserver.NewGitProtocol(
		authorize,
		referenceDiscovery,
		false,
		gitserver.OverallWallTimeHardLimit,
		&gitserver.LibinteractiveCompiler{
			LibinteractiveJarPath: *libinteractivePath,
			Log:                   log,
		},
		log,
	)

	if *rootPath == "" {
		log.Error("root path cannot be empty. Please specify one with -root")
		os.Exit(1)
	}

	gitServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: muxHandler(*rootPath, protocol, log),
	}
	go func() {
		if err := gitServer.ListenAndServe(); err != nil {
			log.Error("gitServer ListenAndServe", "err", err)
		}
	}()
	log.Info(fmt.Sprintf("git server ready for connections at http://localhost:%d", *port))

	var pprofServer *http.Server
	if *pprofPort > 0 {
		pprofServeMux := http.NewServeMux()
		pprofServeMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofServeMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofServeMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofServeMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofServeMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		pprofServer := &http.Server{
			Addr:    fmt.Sprintf("localhost:%d", *pprofPort),
			Handler: pprofServeMux,
		}
		go func() {
			if err := pprofServer.ListenAndServe(); err != nil {
				log.Error("pprof ListenAndServe", "err", err)
			}
		}()
		log.Info(fmt.Sprintf("pprof server ready for connections at http://localhost:%d", *pprofPort))
	}

	<-stopChan

	log.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gitServer.Shutdown(ctx)
	if pprofServer != nil {
		pprofServer.Shutdown(ctx)
	}

	log.Info("Server gracefully stopped.")
}
