package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/charger/eebus"
	"github.com/evcc-io/evcc/cmd/shutdown"
	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/hems"
	"github.com/evcc-io/evcc/meter"
	"github.com/evcc-io/evcc/provider/golang"
	"github.com/evcc-io/evcc/provider/javascript"
	"github.com/evcc-io/evcc/provider/mqtt"
	"github.com/evcc-io/evcc/push"
	"github.com/evcc-io/evcc/server"
	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/server/oauth2redirect"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/locale"
	"github.com/evcc-io/evcc/util/machine"
	"github.com/evcc-io/evcc/util/modbus"
	"github.com/evcc-io/evcc/util/pipe"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/sponsor"
	"github.com/evcc-io/evcc/vehicle"
	"github.com/evcc-io/evcc/vehicle/wrapper"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/libp2p/zeroconf/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
	"golang.org/x/text/currency"
)

var conf = globalConfig{
	Interval: 10 * time.Second,
	Log:      "info",
	Network: networkConfig{
		Schema: "http",
		Host:   "evcc.local",
		Port:   7070,
	},
	Mqtt: mqttConfig{
		Topic: "evcc",
	},
	Database: dbConfig{
		Type: "sqlite",
		Dsn:  "~/.evcc/evcc.db",
	},
}

type globalConfig struct {
	URI          interface{} // TODO deprecated
	Network      networkConfig
	Log          string
	SponsorToken string
	Plant        string // telemetry plant id
	Telemetry    bool
	Metrics      bool
	Profile      bool
	Levels       map[string]string
	Interval     time.Duration
	Database     dbConfig
	Mqtt         mqttConfig
	ModbusProxy  []proxyConfig
	Javascript   []javascriptConfig
	Go           []goConfig
	Influx       server.InfluxConfig
	EEBus        map[string]interface{}
	HEMS         config.Typed
	Messaging    messagingConfig
	Meters       []config.Named
	Chargers     []config.Named
	Vehicles     []config.Named
	Tariffs      tariffConfig
	Site         map[string]interface{}
	Loadpoints   []map[string]interface{}
}

type mqttConfig struct {
	mqtt.Config `mapstructure:",squash"`
	Topic       string
}

type javascriptConfig struct {
	VM     string
	Script string
}

type goConfig struct {
	VM     string
	Script string
}

type proxyConfig struct {
	Port            int
	ReadOnly        bool
	modbus.Settings `mapstructure:",squash"`
}

type dbConfig struct {
	Type string
	Dsn  string
}

type messagingConfig struct {
	Events   map[string]push.EventTemplateConfig
	Services []config.Typed
}

type tariffConfig struct {
	Currency string
	Grid     config.Typed
	FeedIn   config.Typed
	Co2      config.Typed
	Planner  config.Typed
}

type networkConfig struct {
	Schema string
	Host   string
	Port   int
}

func (c networkConfig) HostPort() string {
	if c.Schema == "http" && c.Port == 80 || c.Schema == "https" && c.Port == 443 {
		return c.Host
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func (c networkConfig) URI() string {
	return fmt.Sprintf("%s://%s", c.Schema, c.HostPort())
}

func loadConfigFile(conf *globalConfig) error {
	err := viper.ReadInConfig()

	if cfgFile = viper.ConfigFileUsed(); cfgFile == "" {
		return err
	}

	log.INFO.Println("using config file:", cfgFile)

	if err == nil {
		if err = viper.UnmarshalExact(&conf); err != nil {
			err = fmt.Errorf("failed parsing config file: %w", err)
		}
	}

	// parse log levels after reading config
	if err == nil {
		parseLogLevels()
	}

	return err
}

// devices returns a list of devices from static and configurable device configurations
func devices[T any](static []config.Named, configurable []config.Config) []config.Device[T] {
	res := make([]config.Device[T], 0, len(static)+len(configurable))
	for _, c := range static {
		res = append(res, config.NewStaticDevice[T](c))
	}
	for _, c := range configurable {
		res = append(res, config.NewConfigurableDevice[T](c))
	}
	return res
}

func configureMeters(static []config.Named) error {
	// append devices from database
	configurable, err := config.ConfigurationsByClass(config.Meter)
	if err != nil {
		return err
	}

	for i, dev := range devices[api.Meter](static, configurable) {
		cc := dev.Config()

		if cc.Name == "" {
			return fmt.Errorf("cannot create meter %d: missing name", i+1)
		}

		instance, err := meter.NewFromConfig(cc.Type, cc.Other)
		if err != nil {
			err = fmt.Errorf("cannot create meter '%s': %w", cc.Name, err)
			return err
		}

		dev.Connect(instance)

		if err := config.AddMeter(dev); err != nil {
			return err
		}
	}

	return nil
}

func configureChargers(static []config.Named) error {
	g, _ := errgroup.WithContext(context.Background())

	// append devices from database
	configurable, err := config.ConfigurationsByClass(config.Charger)
	if err != nil {
		return err
	}

	res := devices[api.Charger](static, configurable)

	for i, dev := range res {
		cc := dev.Config()

		if cc.Name == "" {
			return fmt.Errorf("cannot create charger %d: missing name", i+1)
		}

		i := i

		g.Go(func() error {
			instance, err := charger.NewFromConfig(cc.Type, cc.Other)
			if err != nil {
				return fmt.Errorf("cannot create charger '%s': %w", cc.Name, err)
			}

			res[i].Connect(instance)

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	for _, dev := range res {
		if err := config.AddCharger(dev); err != nil {
			return err
		}
	}

	return nil
}

func configureVehicles(static []config.Named) error {
	g, _ := errgroup.WithContext(context.Background())

	// append devices from database
	configurable, err := config.ConfigurationsByClass(config.Vehicle)
	if err != nil {
		return err
	}

	res := devices[api.Vehicle](static, configurable)

	for i, dev := range res {
		cc := dev.Config()

		if cc.Name == "" {
			return fmt.Errorf("cannot create vehicle %d: missing name", i+1)
		}

		i := i

		g.Go(func() error {
			instance, err := vehicle.NewFromConfig(cc.Type, cc.Other)
			if err != nil {
				var ce *util.ConfigError
				if errors.As(err, &ce) {
					return fmt.Errorf("cannot create vehicle '%s': %w", cc.Name, err)
				}

				// wrap non-config vehicle errors to prevent fatals
				log.ERROR.Printf("creating vehicle %s failed: %v", cc.Name, err)
				instance = wrapper.New(cc.Name, cc.Other, err)
			}

			// ensure vehicle config has title
			if instance.Title() == "" {
				//lint:ignore SA1019 as Title is safe on ascii
				instance.SetTitle(strings.Title(cc.Name))
			}

			res[i].Connect(instance)

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	for _, dev := range res {
		if err := config.AddVehicle(dev); err != nil {
			return err
		}
	}

	return nil
}

func configureEnvironment(cmd *cobra.Command, conf globalConfig) (err error) {
	// full http request log
	if cmd.Flags().Lookup(flagHeaders).Changed {
		request.LogHeaders = true
	}

	// setup machine id
	if conf.Plant != "" {
		err = machine.CustomID(conf.Plant)
	}

	// setup sponsorship (allow env override)
	if err == nil && conf.SponsorToken != "" {
		err = sponsor.ConfigureSponsorship(conf.SponsorToken)
	}

	// setup translations
	if err == nil {
		err = locale.Init()
	}

	// setup persistence
	if err == nil && conf.Database.Dsn != "" {
		err = configureDatabase(conf.Database)
	}

	// setup mqtt client listener
	if err == nil && conf.Mqtt.Broker != "" {
		err = configureMQTT(conf.Mqtt)
	}

	// setup javascript VMs
	if err == nil {
		err = configureJavascript(conf.Javascript)
	}

	// setup go VMs
	if err == nil {
		err = configureGo(conf.Go)
	}

	// setup EEBus server
	if err == nil && conf.EEBus != nil {
		err = configureEEBus(conf.EEBus)
	}

	// setup config database
	if err == nil {
		err = config.Init(db.Instance)
	}

	return
}

// configureDatabase configures session database
func configureDatabase(conf dbConfig) error {
	if err := db.NewInstance(conf.Type, conf.Dsn); err != nil {
		return err
	}

	if err := settings.Init(); err != nil {
		return err
	}

	shutdown.Register(func() {
		if err := settings.Persist(); err != nil {
			log.ERROR.Println("cannot save settings:", err)
		}
	})

	return nil
}

// configureInflux configures influx database
func configureInflux(conf server.InfluxConfig, site site.API, in <-chan util.Param) {
	influx := server.NewInfluxClient(
		conf.URL,
		conf.Token,
		conf.Org,
		conf.User,
		conf.Password,
		conf.Database,
	)

	// eliminate duplicate values
	dedupe := pipe.NewDeduplicator(30*time.Minute, "vehicleCapacity", "vehicleSoc", "vehicleRange", "vehicleOdometer", "chargedEnergy", "chargeRemainingEnergy")
	in = dedupe.Pipe(in)

	go influx.Run(site, in)
}

// setup mqtt
func configureMQTT(conf mqttConfig) error {
	log := util.NewLogger("mqtt")

	var err error
	if mqtt.Instance, err = mqtt.RegisteredClient(log, conf.Broker, conf.User, conf.Password, conf.ClientID, 1, conf.Insecure, func(options *paho.ClientOptions) {
		topic := fmt.Sprintf("%s/status", strings.Trim(conf.Topic, "/"))
		options.SetWill(topic, "offline", 1, true)
	}); err != nil {
		return fmt.Errorf("failed configuring mqtt: %w", err)
	}

	return nil
}

// setup javascript
func configureJavascript(conf []javascriptConfig) error {
	for _, cc := range conf {
		if _, err := javascript.RegisteredVM(cc.VM, cc.Script); err != nil {
			return fmt.Errorf("failed configuring javascript: %w", err)
		}
	}
	return nil
}

// setup go
func configureGo(conf []goConfig) error {
	for _, cc := range conf {
		if _, err := golang.RegisteredVM(cc.VM, cc.Script); err != nil {
			return fmt.Errorf("failed configuring go: %w", err)
		}
	}
	return nil
}

// setup HEMS
func configureHEMS(conf config.Typed, site *core.Site, httpd *server.HTTPd) error {
	hems, err := hems.NewFromConfig(conf.Type, conf.Other, site, httpd)
	if err != nil {
		return fmt.Errorf("failed configuring hems: %w", err)
	}

	go hems.Run()

	return nil
}

// setup MDNS
func configureMDNS(conf networkConfig) error {
	host := strings.TrimSuffix(conf.Host, ".local")

	zc, err := zeroconf.RegisterProxy("EV Charge Controller", "_http._tcp", "local.", conf.Port, host, nil, []string{}, nil)
	if err != nil {
		return fmt.Errorf("mDNS announcement: %w", err)
	}

	shutdown.Register(zc.Shutdown)

	return nil
}

// setup EEBus
func configureEEBus(conf map[string]interface{}) error {
	var err error
	if eebus.Instance, err = eebus.NewServer(conf); err != nil {
		return fmt.Errorf("failed configuring eebus: %w", err)
	}

	eebus.Instance.Run()
	shutdown.Register(eebus.Instance.Shutdown)

	return nil
}

// setup messaging
func configureMessengers(conf messagingConfig, valueChan chan util.Param, cache *util.Cache) (chan push.Event, error) {
	messageChan := make(chan push.Event, 1)

	messageHub, err := push.NewHub(conf.Events, cache)
	if err != nil {
		return messageChan, fmt.Errorf("failed configuring push services: %w", err)
	}

	for _, service := range conf.Services {
		impl, err := push.NewFromConfig(service.Type, service.Other)
		if err != nil {
			return messageChan, fmt.Errorf("failed configuring push service %s: %w", service.Type, err)
		}
		messageHub.Add(impl)
	}

	go messageHub.Run(messageChan, valueChan)

	return messageChan, nil
}

func configureTariffs(conf tariffConfig) (tariff.Tariffs, error) {
	var grid, feedin, co2, planner api.Tariff
	var currencyCode currency.Unit = currency.EUR
	var err error

	if conf.Currency != "" {
		currencyCode = currency.MustParseISO(conf.Currency)
	}

	if conf.Grid.Type != "" {
		grid, err = tariff.NewFromConfig(conf.Grid.Type, conf.Grid.Other)
		if err != nil {
			grid = nil
			log.ERROR.Printf("failed configuring grid tariff: %v", err)
		}
	}

	if conf.FeedIn.Type != "" {
		feedin, err = tariff.NewFromConfig(conf.FeedIn.Type, conf.FeedIn.Other)
		if err != nil {
			feedin = nil
			log.ERROR.Printf("failed configuring feed-in tariff: %v", err)
		}
	}

	if conf.Co2.Type != "" {
		co2, err = tariff.NewFromConfig(conf.Co2.Type, conf.Co2.Other)
		if err != nil {
			co2 = nil
			log.ERROR.Printf("failed configuring co2 tariff: %v", err)
		}
	}

	if conf.Planner.Type != "" {
		planner, err = tariff.NewFromConfig(conf.Planner.Type, conf.Planner.Other)
		if err != nil {
			planner = nil
			log.ERROR.Printf("failed configuring planner tariff: %v", err)
		} else if planner.Type() == api.TariffTypeCo2 {
			log.WARN.Printf("tariff configuration changed, use co2 instead of planner for co2 tariff")
		}
	}

	tariffs := tariff.NewTariffs(currencyCode, grid, feedin, co2, planner)

	return *tariffs, nil
}

func configureDevices(conf globalConfig) error {
	if err := configureMeters(conf.Meters); err != nil {
		return err
	}
	if err := configureChargers(conf.Chargers); err != nil {
		return err
	}
	return configureVehicles(conf.Vehicles)
}

func configureSiteAndLoadpoints(conf globalConfig) (*core.Site, error) {
	if err := configureDevices(conf); err != nil {
		return nil, err
	}

	loadpoints, err := configureLoadpoints(conf)
	if err != nil {
		return nil, fmt.Errorf("failed configuring loadpoints: %w", err)
	}

	tariffs, err := configureTariffs(conf.Tariffs)
	if err != nil {
		return nil, err
	}

	return configureSite(conf.Site, loadpoints, config.Instances(config.Vehicles()), tariffs)
}

func configureSite(conf map[string]interface{}, loadpoints []*core.Loadpoint, vehicles []api.Vehicle, tariffs tariff.Tariffs) (*core.Site, error) {
	site, err := core.NewSiteFromConfig(log, conf, loadpoints, vehicles, tariffs)
	if err != nil {
		return nil, fmt.Errorf("failed configuring site: %w", err)
	}

	return site, nil
}

func configureLoadpoints(conf globalConfig) (loadpoints []*core.Loadpoint, err error) {
	lpInterfaces, ok := viper.AllSettings()["loadpoints"].([]interface{})
	if !ok || len(lpInterfaces) == 0 {
		return nil, errors.New("missing loadpoints")
	}

	for id, lpcI := range lpInterfaces {
		var lpc map[string]interface{}
		if err := util.DecodeOther(lpcI, &lpc); err != nil {
			return nil, fmt.Errorf("failed decoding loadpoint configuration: %w", err)
		}

		log := util.NewLogger("lp-" + strconv.Itoa(id+1))
		lp, err := core.NewLoadpointFromConfig(log, lpc)
		if err != nil {
			return nil, fmt.Errorf("failed configuring loadpoint: %w", err)
		}

		loadpoints = append(loadpoints, lp)
	}

	return loadpoints, nil
}

// configureAuth handles routing for devices. For now only api.AuthProvider related routes
func configureAuth(conf networkConfig, vehicles []api.Vehicle, router *mux.Router, paramC chan<- util.Param) {
	auth := router.PathPrefix("/oauth").Subrouter()
	auth.Use(handlers.CompressHandler)
	auth.Use(handlers.CORS(
		handlers.AllowedHeaders([]string{"Content-Type"}),
	))

	// wire the handler
	oauth2redirect.SetupRouter(auth)

	// initialize
	authCollection := util.NewAuthCollection(paramC)

	baseURI := conf.URI()
	baseAuthURI := fmt.Sprintf("%s/oauth", baseURI)

	var id int
	for _, v := range vehicles {
		if provider, ok := v.(api.AuthProvider); ok {
			id += 1

			basePath := fmt.Sprintf("vehicles/%d", id)
			callbackURI := fmt.Sprintf("%s/%s/callback", baseAuthURI, basePath)

			// register vehicle
			ap := authCollection.Register(fmt.Sprintf("oauth/%s", basePath), v.Title())

			provider.SetCallbackParams(baseURI, callbackURI, ap.Handler())

			auth.
				Methods(http.MethodPost).
				Path(fmt.Sprintf("/%s/login", basePath)).
				HandlerFunc(provider.LoginHandler())
			auth.
				Methods(http.MethodPost).
				Path(fmt.Sprintf("/%s/logout", basePath)).
				HandlerFunc(provider.LogoutHandler())

			log.INFO.Printf("ensure the oauth client redirect/callback is configured for %s: %s", v.Title(), callbackURI)
		}
	}

	authCollection.Publish()
}
