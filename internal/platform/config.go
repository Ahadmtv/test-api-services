package platform

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrMissingConfig is returned when required environment configuration is absent.
var ErrMissingConfig = errors.New("required configuration is missing")

type Config struct {
	Env          string
	HTTP         HTTPConfig
	DB           DBConfig
	Log          LogConfig
	Auth         AuthConfig
	Availability AvailabilityConfig
	Booking      BookingConfig
	Payouts      PayoutConfig
	Payments     PaymentConfig
	Images       ImagesConfig
	Version      string
}

// ImagesConfig controls server-side image validation, resizing and storage.
type ImagesConfig struct {
	StorageDir          string
	MaxUploadBytes      int64
	MaxInputDimension   int
	MaxInputPixels      int64
	ProfileMaxDimension int
	CoverMaxDimension   int
}

type AvailabilityConfig struct {
	SlotIncrement  time.Duration
	MinimumNotice  time.Duration
	MaximumHorizon time.Duration
}

type BookingConfig struct {
	CancellationCutoff time.Duration
	// CompletionDisputeWindow is the window the client has to dispute a
	// completion request before it settles automatically.
	CompletionDisputeWindow time.Duration
	// PendingPaymentTTL is how long gateway-funded bookings reserve their slot.
	PendingPaymentTTL time.Duration
	ExpiryGrace       time.Duration
}

type PayoutConfig struct {
	MinAmountIRR int64
}

type PaymentConfig struct {
	Environment string
	MerchantID  string
	CallbackURL string
	ReturnURL   string
}

type AuthConfig struct {
	AccessTokenSecret  string
	AccessTokenTTL     time.Duration
	RefreshTokenTTL    time.Duration
	RateLimitPerMinute int
}

type HTTPConfig struct {
	Host         string
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	BodyLimit    int
}

type DBConfig struct {
	Host            string
	Port            int
	User            string
	Password        string
	Name            string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	MaxConnIdleTime time.Duration
	MaxConnLifetime time.Duration
}

type LogConfig struct {
	Level  string
	Format string
}

const (
	defaultEnv                       = "development"
	defaultVersion                   = "0.1.0"
	defaultHTTPHost                  = "0.0.0.0"
	defaultHTTPPort                  = 8080
	defaultReadTimeout               = 10 * time.Second
	defaultWriteTimeout              = 10 * time.Second
	defaultIdleTimeout               = 30 * time.Second
	defaultHTTPBodyLimit             = 1 << 20
	defaultDBHost                    = "localhost"
	defaultDBPort                    = 5432
	defaultDBSSLMode                 = "disable"
	defaultDBMaxOpenConns            = 25
	defaultDBMaxIdleConns            = 5
	defaultDBMaxConnIdle             = 10 * time.Minute
	defaultDBMaxConnLife             = 1 * time.Hour
	defaultLogLevel                  = "info"
	defaultLogFormat                 = "json"
	defaultAuthAccessTokenTTL        = 15 * time.Minute
	defaultAuthRefreshTokenTTL       = 168 * time.Hour
	defaultAuthRateLimitPerMinute    = 30
	defaultAvailabilitySlot          = 15 * time.Minute
	defaultAvailabilityNotice        = 2 * time.Hour
	defaultAvailabilityHorizon       = 90 * 24 * time.Hour
	defaultDisputeWindow             = 72 * time.Hour
	defaultPendingPaymentTTL         = 15 * time.Minute
	defaultBookingPaymentExpiryGrace = 15 * time.Minute
	defaultPayoutMinAmountIRR        = int64(100_000)
	minAuthAccessTokenSecretLen      = 32
	defaultImageStorageDir           = "static/img"
	defaultImageMaxUploadBytes       = int64(5 << 20)
	defaultImageMaxDimension         = 8000
	defaultImageMaxPixels            = int64(40_000_000)
	defaultProfileMaxDimension       = 512
	defaultCoverMaxDimension         = 1600
	maxConfiguredImageUploadBytes    = int64(25 << 20)
)

// LoadConfig reads configuration from environment variables and validates it.
// Required variables that are missing are reported explicitly; no secret is
// ever logged.
func LoadConfig() (*Config, error) {
	var err error
	cfg := &Config{
		Env:     firstNonEmpty(os.Getenv("APP_ENV"), defaultEnv),
		Version: firstNonEmpty(os.Getenv("APP_VERSION"), defaultVersion),
		HTTP: HTTPConfig{
			Host:         firstNonEmpty(os.Getenv("HTTP_HOST"), defaultHTTPHost),
			Port:         parseInt(os.Getenv("HTTP_PORT"), defaultHTTPPort, "HTTP_PORT"),
			ReadTimeout:  parseDuration(os.Getenv("HTTP_READ_TIMEOUT"), defaultReadTimeout, "HTTP_READ_TIMEOUT"),
			WriteTimeout: parseDuration(os.Getenv("HTTP_WRITE_TIMEOUT"), defaultWriteTimeout, "HTTP_WRITE_TIMEOUT"),
			IdleTimeout:  parseDuration(os.Getenv("HTTP_IDLE_TIMEOUT"), defaultIdleTimeout, "HTTP_IDLE_TIMEOUT"),
			BodyLimit:    parsePositiveInt(os.Getenv("HTTP_BODY_LIMIT"), defaultHTTPBodyLimit),
		},
		DB: DBConfig{
			Host:            firstNonEmpty(os.Getenv("DB_HOST"), defaultDBHost),
			Port:            parseInt(os.Getenv("DB_PORT"), defaultDBPort, "DB_PORT"),
			User:            os.Getenv("DB_USER"),
			Password:        os.Getenv("DB_PASSWORD"),
			Name:            os.Getenv("DB_NAME"),
			SSLMode:         firstNonEmpty(os.Getenv("DB_SSL_MODE"), defaultDBSSLMode),
			MaxOpenConns:    parseInt(os.Getenv("DB_MAX_OPEN_CONNS"), defaultDBMaxOpenConns, "DB_MAX_OPEN_CONNS"),
			MaxIdleConns:    parseInt(os.Getenv("DB_MAX_IDLE_CONNS"), defaultDBMaxIdleConns, "DB_MAX_IDLE_CONNS"),
			MaxConnIdleTime: parseDuration(os.Getenv("DB_MAX_CONN_IDLE_TIME"), defaultDBMaxConnIdle, "DB_MAX_CONN_IDLE_TIME"),
			MaxConnLifetime: parseDuration(os.Getenv("DB_MAX_CONN_LIFETIME"), defaultDBMaxConnLife, "DB_MAX_CONN_LIFETIME"),
		},
		Log: LogConfig{
			Level:  firstNonEmpty(os.Getenv("LOG_LEVEL"), defaultLogLevel),
			Format: firstNonEmpty(os.Getenv("LOG_FORMAT"), defaultLogFormat),
		},
	}

	cfg.Payments = PaymentConfig{
		Environment: firstNonEmpty(os.Getenv("PAYMENT_ENVIRONMENT"), "sandbox"),
		MerchantID:  os.Getenv("ZARINPAL_MERCHANT_ID"),
		CallbackURL: os.Getenv("PAYMENT_CALLBACK_URL"),
		ReturnURL:   os.Getenv("PAYMENT_RETURN_URL"),
	}

	if missing := requiredMissing(
		"DB_USER", cfg.DB.User,
		"DB_NAME", cfg.DB.Name,
	); len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingConfig, strings.Join(missing, ", "))
	}

	if cfg.Payments.Environment != "sandbox" && cfg.Payments.Environment != "production" {
		return nil, fmt.Errorf("PAYMENT_ENVIRONMENT must be sandbox or production")
	}
	if cfg.Payments.CallbackURL != "" && !isAbsoluteHTTPURL(cfg.Payments.CallbackURL) {
		return nil, fmt.Errorf("PAYMENT_CALLBACK_URL must be an absolute HTTP URL")
	}
	if cfg.Payments.ReturnURL != "" && !isAbsoluteHTTPURL(cfg.Payments.ReturnURL) {
		return nil, fmt.Errorf("PAYMENT_RETURN_URL must be an absolute HTTP URL")
	}
	if cfg.Payments.Environment == "production" && cfg.Payments.ReturnURL == "" {
		return nil, fmt.Errorf("%w: PAYMENT_RETURN_URL", ErrMissingConfig)
	}

	if cfg.Log.Format != "json" && cfg.Log.Format != "text" {
		return nil, fmt.Errorf("invalid LOG_FORMAT %q (expected json or text)", cfg.Log.Format)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		return nil, fmt.Errorf("invalid LOG_LEVEL %q: %w", cfg.Log.Level, err)
	}
	_ = level

	cfg.Auth = AuthConfig{
		AccessTokenSecret:  os.Getenv("AUTH_ACCESS_TOKEN_SECRET"),
		AccessTokenTTL:     parseDuration(os.Getenv("AUTH_ACCESS_TOKEN_TTL"), defaultAuthAccessTokenTTL, "AUTH_ACCESS_TOKEN_TTL"),
		RefreshTokenTTL:    parseDuration(os.Getenv("AUTH_REFRESH_TOKEN_TTL"), defaultAuthRefreshTokenTTL, "AUTH_REFRESH_TOKEN_TTL"),
		RateLimitPerMinute: parseInt(os.Getenv("AUTH_RATE_LIMIT_PER_MIN"), defaultAuthRateLimitPerMinute, "AUTH_RATE_LIMIT_PER_MIN"),
	}

	if strings.TrimSpace(cfg.Auth.AccessTokenSecret) == "" {
		return nil, fmt.Errorf("%w: AUTH_ACCESS_TOKEN_SECRET", ErrMissingConfig)
	}
	if len(cfg.Auth.AccessTokenSecret) < minAuthAccessTokenSecretLen {
		return nil, fmt.Errorf("AUTH_ACCESS_TOKEN_SECRET must be at least %d characters", minAuthAccessTokenSecretLen)
	}
	if cfg.Auth.AccessTokenTTL <= 0 {
		return nil, fmt.Errorf("AUTH_ACCESS_TOKEN_TTL must be positive")
	}
	if cfg.Auth.RefreshTokenTTL <= 0 {
		return nil, fmt.Errorf("AUTH_REFRESH_TOKEN_TTL must be positive")
	}
	if cfg.Auth.RateLimitPerMinute <= 0 {
		return nil, fmt.Errorf("AUTH_RATE_LIMIT_PER_MIN must be positive")
	}

	cfg.Availability.SlotIncrement, err = parseAvailabilityDuration("AVAILABILITY_SLOT_INCREMENT", defaultAvailabilitySlot, false)
	if err != nil {
		return nil, err
	}
	cfg.Availability.MinimumNotice, err = parseAvailabilityDuration("AVAILABILITY_MIN_NOTICE", defaultAvailabilityNotice, true)
	if err != nil {
		return nil, err
	}
	cfg.Availability.MaximumHorizon, err = parseAvailabilityDuration("AVAILABILITY_MAX_HORIZON", defaultAvailabilityHorizon, false)
	if err != nil {
		return nil, err
	}
	if cfg.Availability.SlotIncrement%time.Minute != 0 {
		return nil, fmt.Errorf("AVAILABILITY_SLOT_INCREMENT must be a whole number of minutes")
	}
	if cfg.Availability.MaximumHorizon <= cfg.Availability.MinimumNotice {
		return nil, fmt.Errorf("AVAILABILITY_MAX_HORIZON must exceed AVAILABILITY_MIN_NOTICE")
	}

	cfg.Booking.CancellationCutoff, err = parseAvailabilityDuration("BOOKING_CANCELLATION_CUTOFF", 0, true)
	if err != nil {
		return nil, err
	}
	cfg.Booking.CompletionDisputeWindow, err = parseAvailabilityDuration("BOOKING_COMPLETION_DISPUTE_WINDOW", defaultDisputeWindow, false)
	if err != nil {
		return nil, err
	}
	cfg.Booking.PendingPaymentTTL, err = parseAvailabilityDuration("BOOKING_PENDING_PAYMENT_TTL", defaultPendingPaymentTTL, false)
	if err != nil {
		return nil, err
	}
	cfg.Booking.ExpiryGrace, err = parseAvailabilityDuration("BOOKING_PAYMENT_EXPIRY_GRACE", defaultBookingPaymentExpiryGrace, true)
	if err != nil {
		return nil, err
	}

	cfg.Payouts.MinAmountIRR, err = parseIrrMinimum(os.Getenv("PAYOUT_MIN_AMOUNT_IRR"), defaultPayoutMinAmountIRR)
	if err != nil {
		return nil, err
	}

	cfg.Images = ImagesConfig{
		StorageDir:          firstNonEmpty(os.Getenv("IMAGE_STORAGE_DIR"), defaultImageStorageDir),
		MaxUploadBytes:      parseInt64(os.Getenv("IMAGE_MAX_UPLOAD_BYTES"), defaultImageMaxUploadBytes),
		MaxInputDimension:   parseInt(os.Getenv("IMAGE_MAX_INPUT_DIMENSION"), defaultImageMaxDimension, "IMAGE_MAX_INPUT_DIMENSION"),
		MaxInputPixels:      parseInt64(os.Getenv("IMAGE_MAX_INPUT_PIXELS"), defaultImageMaxPixels),
		ProfileMaxDimension: parseInt(os.Getenv("IMAGE_PROFILE_MAX_DIMENSION"), defaultProfileMaxDimension, "IMAGE_PROFILE_MAX_DIMENSION"),
		CoverMaxDimension:   parseInt(os.Getenv("IMAGE_COVER_MAX_DIMENSION"), defaultCoverMaxDimension, "IMAGE_COVER_MAX_DIMENSION"),
	}
	if cfg.Images.MaxUploadBytes < 1 || cfg.Images.MaxUploadBytes > maxConfiguredImageUploadBytes {
		return nil, fmt.Errorf("IMAGE_MAX_UPLOAD_BYTES must be between 1 and %d", maxConfiguredImageUploadBytes)
	}
	if cfg.Images.MaxInputDimension < 1 || cfg.Images.MaxInputPixels < 1 {
		return nil, fmt.Errorf("IMAGE_MAX_INPUT_DIMENSION and IMAGE_MAX_INPUT_PIXELS must be positive")
	}
	if cfg.Images.ProfileMaxDimension < 1 || cfg.Images.CoverMaxDimension <= cfg.Images.ProfileMaxDimension {
		return nil, fmt.Errorf("IMAGE_COVER_MAX_DIMENSION must exceed IMAGE_PROFILE_MAX_DIMENSION")
	}

	return cfg, nil
}

// HTTPBodyLimitForUploads returns the transport body cap needed to accept one
// image plus multipart overhead while retaining HTTP_BODY_LIMIT for all other
// endpoints through the route-aware body guard.
func (c *Config) HTTPBodyLimitForUploads() int {
	limit := c.Images.MaxUploadBytes + (64 << 10)
	if int64(c.HTTP.BodyLimit) > limit {
		return c.HTTP.BodyLimit
	}
	return int(limit)
}

// parseIrrMinimum reads an integer IRR threshold strictly greater than one.
func parseIrrMinimum(value string, fallback int64) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 2 {
		return 0, fmt.Errorf("PAYOUT_MIN_AMOUNT_IRR must be an integer greater than one")
	}
	return parsed, nil
}

// DSN builds a libpq-style connection string for pgx from configuration parts.
func (c DBConfig) DSN() string {
	var b strings.Builder
	b.WriteString("postgres://")
	if c.User != "" {
		b.WriteString(c.User)
	}
	if c.Password != "" {
		b.WriteString(":")
		b.WriteString(c.Password)
	}
	b.WriteString("@")
	b.WriteString(c.Host)
	b.WriteString(":")
	b.WriteString(strconv.Itoa(c.Port))
	b.WriteString("/")
	b.WriteString(c.Name)
	b.WriteString("?sslmode=")
	b.WriteString(c.SSLMode)
	return b.String()
}

func isAbsoluteHTTPURL(value string) bool {
	if strings.Contains(value, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func requiredMissing(namesAndValues ...string) []string {
	var missing []string
	for i := 0; i+1 < len(namesAndValues); i += 2 {
		if strings.TrimSpace(namesAndValues[i+1]) == "" {
			missing = append(missing, namesAndValues[i])
		}
	}
	return missing
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func parseInt(value string, fallback int, name string) int {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func parseInt64(value string, fallback int64) int64 {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// parsePositiveInt reads a strictly positive integer, falling back when the
// value is absent or non-positive.
func parsePositiveInt(value string, fallback int) int {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func parseAvailabilityDuration(name string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration", name)
	}
	if duration < 0 || (!allowZero && duration == 0) {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return duration, nil
}

func parseDuration(value string, fallback time.Duration, name string) time.Duration {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return d
}
