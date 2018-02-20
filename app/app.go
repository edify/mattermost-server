// Copyright (c) 2016-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package app

import (
	"html/template"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	l4g "github.com/alecthomas/log4go"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/einterfaces"
	ejobs "github.com/mattermost/mattermost-server/einterfaces/jobs"
	"github.com/mattermost/mattermost-server/jobs"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/plugin/pluginenv"
	"github.com/mattermost/mattermost-server/store"
	"github.com/mattermost/mattermost-server/store/sqlstore"
	"github.com/mattermost/mattermost-server/store/messagingapi"
	"github.com/mattermost/mattermost-server/utils"
)

type App struct {
	goroutineCount      int32
	goroutineExitSignal chan struct{}

	Srv *Server

	PluginEnv              *pluginenv.Environment
	PluginConfigListenerId string

	EmailBatching *EmailBatchingJob

	Hubs                        []*Hub
	HubsStopCheckingForDeadlock chan bool

	Jobs *jobs.JobServer

	EsisJobs *jobs.EsisJobsServer

	AccountMigration einterfaces.AccountMigrationInterface
	Brand            einterfaces.BrandInterface
	Cluster          einterfaces.ClusterInterface
	Compliance       einterfaces.ComplianceInterface
	DataRetention    einterfaces.DataRetentionInterface
	Elasticsearch    einterfaces.ElasticsearchInterface
	Emoji            einterfaces.EmojiInterface
	Ldap             einterfaces.LdapInterface
	MessageExport    einterfaces.MessageExportInterface
	Metrics          einterfaces.MetricsInterface
	Mfa              einterfaces.MfaInterface
	Saml             einterfaces.SamlInterface

	config          atomic.Value
	configFile      string
	configListeners map[string]func(*model.Config, *model.Config)

	newStore func() store.Store

	htmlTemplateWatcher *utils.HTMLTemplateWatcher
	sessionCache        *utils.Cache
	roles               map[string]*model.Role
	configListenerId    string
	licenseListenerId   string
	disableConfigWatch  bool
	configWatcher       *utils.ConfigWatcher

	pluginCommands     []*PluginCommand
	pluginCommandsLock sync.RWMutex

	clientConfig     map[string]string
	clientConfigHash string
	diagnosticId     string
}

var appCount = 0

// New creates a new App. You must call Shutdown when you're done with it.
// XXX: For now, only one at a time is allowed as some resources are still shared.
func New(options ...Option) (*App, error) {
	appCount++
	if appCount > 1 {
		panic("Only one App should exist at a time. Did you forget to call Shutdown()?")
	}

	app := &App{
		goroutineExitSignal: make(chan struct{}, 1),
		Srv: &Server{
			Router: mux.NewRouter(),
		},
		sessionCache:    utils.NewLru(model.SESSION_CACHE_SIZE),
		configFile:      "config.json",
		configListeners: make(map[string]func(*model.Config, *model.Config)),
		clientConfig:    make(map[string]string),
	}

	for _, option := range options {
		option(app)
	}

	if utils.T == nil {
		if err := utils.TranslationsPreInit(); err != nil {
			return nil, errors.Wrapf(err, "unable to load Mattermost translation files")
		}
	}
	model.AppErrorInit(utils.T)
	if err := app.LoadConfig(app.configFile); err != nil {
		return nil, err
	}
	app.EnableConfigWatch()
	if err := utils.InitTranslations(app.Config().LocalizationSettings); err != nil {
		return nil, errors.Wrapf(err, "unable to load Mattermost translation files")
	}

	app.configListenerId = app.AddConfigListener(func(_, _ *model.Config) {
		app.configOrLicenseListener()
	})
	app.licenseListenerId = utils.AddLicenseListener(app.configOrLicenseListener)
	app.regenerateClientConfig()
	app.SetDefaultRolesBasedOnConfig()

	l4g.Info(utils.T("api.server.new_server.init.info"))

	app.initEnterprise()

	if app.newStore == nil {
		app.newStore = func() store.Store {
			return store.NewLayeredStore(sqlstore.NewSqlSupplier(app.Config().SqlSettings, app.Metrics), app.Metrics, app.Cluster)
		}
	}

	if htmlTemplateWatcher, err := utils.NewHTMLTemplateWatcher("templates"); err != nil {
		l4g.Error(utils.T("api.api.init.parsing_templates.error"), err)
	} else {
		app.htmlTemplateWatcher = htmlTemplateWatcher
	}

	app.Srv.Store = app.newStore()
	app.initJobs()

	app.EsisJobs = jobs.NewEsisJobsServer(app.Srv.Store)
	app.initBuiltInPlugins()
	app.Srv.Router.HandleFunc("/plugins/{plugin_id:[A-Za-z0-9\\_\\-\\.]+}", app.ServePluginRequest)
	app.Srv.Router.HandleFunc("/plugins/{plugin_id:[A-Za-z0-9\\_\\-\\.]+}/{anything:.*}", app.ServePluginRequest)

	app.Srv.Router.NotFoundHandler = http.HandlerFunc(app.Handle404)

	app.Srv.WebSocketRouter = &WebSocketRouter{
		app:      app,
		handlers: make(map[string]webSocketHandler),
	}

	app.Srv.ApiStore = messagingapi.NewApiStore(&app.Config().MessagingApiSettings, app.HTTPClient(true))

	return app, nil
}

func (a *App) configOrLicenseListener() {
	a.regenerateClientConfig()
	a.SetDefaultRolesBasedOnConfig()
}

func (a *App) Shutdown() {
	appCount--

	l4g.Info(utils.T("api.server.stop_server.stopping.info"))

	a.StopServer()
	a.HubStop()

	a.ShutDownPlugins()
	a.WaitForGoroutines()

	a.Srv.Store.Close()
	a.Srv = nil

	if a.htmlTemplateWatcher != nil {
		a.htmlTemplateWatcher.Close()
	}

	a.RemoveConfigListener(a.configListenerId)
	utils.RemoveLicenseListener(a.licenseListenerId)
	l4g.Info(utils.T("api.server.stop_server.stopped.info"))

	a.DisableConfigWatch()
}

var accountMigrationInterface func(*App) einterfaces.AccountMigrationInterface

func RegisterAccountMigrationInterface(f func(*App) einterfaces.AccountMigrationInterface) {
	accountMigrationInterface = f
}

var brandInterface func(*App) einterfaces.BrandInterface

func RegisterBrandInterface(f func(*App) einterfaces.BrandInterface) {
	brandInterface = f
}

var clusterInterface func(*App) einterfaces.ClusterInterface

func RegisterClusterInterface(f func(*App) einterfaces.ClusterInterface) {
	clusterInterface = f
}

var complianceInterface func(*App) einterfaces.ComplianceInterface

func RegisterComplianceInterface(f func(*App) einterfaces.ComplianceInterface) {
	complianceInterface = f
}

var dataRetentionInterface func(*App) einterfaces.DataRetentionInterface

func RegisterDataRetentionInterface(f func(*App) einterfaces.DataRetentionInterface) {
	dataRetentionInterface = f
}

var elasticsearchInterface func(*App) einterfaces.ElasticsearchInterface

func RegisterElasticsearchInterface(f func(*App) einterfaces.ElasticsearchInterface) {
	elasticsearchInterface = f
}

var emojiInterface func(*App) einterfaces.EmojiInterface

func RegisterEmojiInterface(f func(*App) einterfaces.EmojiInterface) {
	emojiInterface = f
}

var jobsDataRetentionJobInterface func(*App) ejobs.DataRetentionJobInterface

func RegisterJobsDataRetentionJobInterface(f func(*App) ejobs.DataRetentionJobInterface) {
	jobsDataRetentionJobInterface = f
}

var jobsMessageExportJobInterface func(*App) ejobs.MessageExportJobInterface

func RegisterJobsMessageExportJobInterface(f func(*App) ejobs.MessageExportJobInterface) {
	jobsMessageExportJobInterface = f
}

var jobsElasticsearchAggregatorInterface func(*App) ejobs.ElasticsearchAggregatorInterface

func RegisterJobsElasticsearchAggregatorInterface(f func(*App) ejobs.ElasticsearchAggregatorInterface) {
	jobsElasticsearchAggregatorInterface = f
}

var jobsElasticsearchIndexerInterface func(*App) ejobs.ElasticsearchIndexerInterface

func RegisterJobsElasticsearchIndexerInterface(f func(*App) ejobs.ElasticsearchIndexerInterface) {
	jobsElasticsearchIndexerInterface = f
}

var jobsLdapSyncInterface func(*App) ejobs.LdapSyncInterface

func RegisterJobsLdapSyncInterface(f func(*App) ejobs.LdapSyncInterface) {
	jobsLdapSyncInterface = f
}

var ldapInterface func(*App) einterfaces.LdapInterface

func RegisterLdapInterface(f func(*App) einterfaces.LdapInterface) {
	ldapInterface = f
}

var messageExportInterface func(*App) einterfaces.MessageExportInterface

func RegisterMessageExportInterface(f func(*App) einterfaces.MessageExportInterface) {
	messageExportInterface = f
}

var metricsInterface func(*App) einterfaces.MetricsInterface

func RegisterMetricsInterface(f func(*App) einterfaces.MetricsInterface) {
	metricsInterface = f
}

var mfaInterface func(*App) einterfaces.MfaInterface

func RegisterMfaInterface(f func(*App) einterfaces.MfaInterface) {
	mfaInterface = f
}

var samlInterface func(*App) einterfaces.SamlInterface

func RegisterSamlInterface(f func(*App) einterfaces.SamlInterface) {
	samlInterface = f
}

func (a *App) initEnterprise() {
	if accountMigrationInterface != nil {
		a.AccountMigration = accountMigrationInterface(a)
	}
	if brandInterface != nil {
		a.Brand = brandInterface(a)
	}
	if clusterInterface != nil {
		a.Cluster = clusterInterface(a)
	}
	if complianceInterface != nil {
		a.Compliance = complianceInterface(a)
	}
	if elasticsearchInterface != nil {
		a.Elasticsearch = elasticsearchInterface(a)
	}
	if emojiInterface != nil {
		a.Emoji = emojiInterface(a)
	}
	if ldapInterface != nil {
		a.Ldap = ldapInterface(a)
		a.AddConfigListener(func(_, cfg *model.Config) {
			if err := utils.ValidateLdapFilter(cfg, a.Ldap); err != nil {
				panic(utils.T(err.Id))
			}
		})
	}
	if messageExportInterface != nil {
		a.MessageExport = messageExportInterface(a)
	}
	if metricsInterface != nil {
		a.Metrics = metricsInterface(a)
	}
	if mfaInterface != nil {
		a.Mfa = mfaInterface(a)
	}
	if samlInterface != nil {
		a.Saml = samlInterface(a)
		a.AddConfigListener(func(_, cfg *model.Config) {
			a.Saml.ConfigureSP()
		})
	}
	if dataRetentionInterface != nil {
		a.DataRetention = dataRetentionInterface(a)
	}
}

func (a *App) initJobs() {
	a.Jobs = jobs.NewJobServer(a, a.Srv.Store)
	if jobsDataRetentionJobInterface != nil {
		a.Jobs.DataRetentionJob = jobsDataRetentionJobInterface(a)
	}
	if jobsMessageExportJobInterface != nil {
		a.Jobs.MessageExportJob = jobsMessageExportJobInterface(a)
	}
	if jobsElasticsearchAggregatorInterface != nil {
		a.Jobs.ElasticsearchAggregator = jobsElasticsearchAggregatorInterface(a)
	}
	if jobsElasticsearchIndexerInterface != nil {
		a.Jobs.ElasticsearchIndexer = jobsElasticsearchIndexerInterface(a)
	}
	if jobsLdapSyncInterface != nil {
		a.Jobs.LdapSync = jobsLdapSyncInterface(a)
	}
}

func (a *App) DiagnosticId() string {
	return a.diagnosticId
}

func (a *App) SetDiagnosticId(id string) {
	a.diagnosticId = id
}

func (a *App) EnsureDiagnosticId() {
	if a.diagnosticId != "" {
		return
	}
	if result := <-a.Srv.Store.System().Get(); result.Err == nil {
		props := result.Data.(model.StringMap)

		id := props[model.SYSTEM_DIAGNOSTIC_ID]
		if len(id) == 0 {
			id = model.NewId()
			systemId := &model.System{Name: model.SYSTEM_DIAGNOSTIC_ID, Value: id}
			<-a.Srv.Store.System().Save(systemId)
		}

		a.diagnosticId = id
	}
}

// Go creates a goroutine, but maintains a record of it to ensure that execution completes before
// the app is destroyed.
func (a *App) Go(f func()) {
	atomic.AddInt32(&a.goroutineCount, 1)

	go func() {
		f()

		atomic.AddInt32(&a.goroutineCount, -1)
		select {
		case a.goroutineExitSignal <- struct{}{}:
		default:
		}
	}()
}

// WaitForGoroutines blocks until all goroutines created by App.Go exit.
func (a *App) WaitForGoroutines() {
	for atomic.LoadInt32(&a.goroutineCount) != 0 {
		<-a.goroutineExitSignal
	}
}

func (a *App) HTMLTemplates() *template.Template {
	return a.htmlTemplateWatcher.Templates()
}

func (a *App) HTTPClient(trustURLs bool) *http.Client {
	insecure := a.Config().ServiceSettings.EnableInsecureOutgoingConnections != nil && *a.Config().ServiceSettings.EnableInsecureOutgoingConnections

	if trustURLs {
		return utils.NewHTTPClient(insecure, nil, nil)
	}

	allowHost := func(host string) bool {
		if a.Config().ServiceSettings.AllowedUntrustedInternalConnections == nil {
			return false
		}
		for _, allowed := range strings.Fields(*a.Config().ServiceSettings.AllowedUntrustedInternalConnections) {
			if host == allowed {
				return true
			}
		}
		return false
	}

	allowIP := func(ip net.IP) bool {
		if !utils.IsReservedIP(ip) {
			return true
		}
		if a.Config().ServiceSettings.AllowedUntrustedInternalConnections == nil {
			return false
		}
		for _, allowed := range strings.Fields(*a.Config().ServiceSettings.AllowedUntrustedInternalConnections) {
			if _, ipRange, err := net.ParseCIDR(allowed); err == nil && ipRange.Contains(ip) {
				return true
			}
		}
		return false
	}

	return utils.NewHTTPClient(insecure, allowHost, allowIP)
}

func (a *App) Handle404(w http.ResponseWriter, r *http.Request) {
	err := model.NewAppError("Handle404", "api.context.404.app_error", nil, "", http.StatusNotFound)

	l4g.Debug("%v: code=404 ip=%v", r.URL.Path, utils.GetIpAddress(r))

	utils.RenderWebError(err, w, r)
}
