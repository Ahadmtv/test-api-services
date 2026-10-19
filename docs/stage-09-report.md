# گزارش مرحله 09 — آزمون یکپارچه نهایی، سخت‌سازی و راستی‌آزمایی

این گزارش وضعیت واقعی مرحله 09 را مستند می‌کند. تنها مواردی «موفق» ثبت شده‌اند که در همین محیط اجرا و با موفقیت تمام شده‌اند؛ موارد اجرانشده یا مسدود جداگانه ذکر شده‌اند.

## دامنه

- اجرای آزمون یکپارچه سرتاسری روی PostgreSQL واقعی و مسیر HTTP واقعی (Fiber).
- راستی‌آزمایی هم‌زمانی و صحت مالی گذارهای پرخطر (تسویه تکمیل و بازپرداخت).
- بستن شکاف‌های امنیتی/سخت‌سازی مهلت‌دار بدون بازنویسی رفتار موجود.
- همگام‌سازی قرارداد OpenAPI 3.1 و مستندات فارسی با پیاده‌سازی.

طبق ADRهای موجود، رفتار دامنه تغییر نکرده است؛ این مرحله فقط شکاف‌ها را می‌بندد و صحت را اثبات می‌کند.

## تغییرهای انجام‌شده

### ۱) چرخش اتمی توکن Refresh

- `internal/identity/store.go`: افزودن `FindRefreshTokenForUpdate` (`SELECT ... FOR UPDATE`) و تغییر `RevokeRefreshToken` به بازگرداندن تعداد ردیف‌های تأثیرگرفته.
- `internal/identity/service.go`: بازنویسی `Refresh` به‌صورت اتمی؛ قفل ردیف، تولید توکن جایگزین، لغو توکن قبلی با پیوند `replaced_by`، و شناسایی replay (توکن لغوشده با جایگزین) که منجر به `RevokeAllUserTokens` و خطای `ErrTokenReplayDetected` می‌شود. توکن دسترسی تازه اکنون ایمیل و نقش‌های جاری کاربر را حفظ می‌کند (پیش‌تر در چرخش از دست می‌رفت). صدور توکن در `Register` و `Login` نیز به همان تراکنش منتقل شد (`issueTokenPairTx`).
- آزمون‌های واحد/یکپارچه مرتبط به‌روزرسانی شدند.

### ۲) سخت‌سازی پرداخت و بدنه درخواست

- `internal/payments/handler.go`: امضای `NewHandler(service, *auth.RateLimiter)` و اعمال محدودیت نرخ روی `POST /api/v1/payments/zarinpal` با کلید `payments:create` و کد خطای `rate_limited` (پاسخ `429`).
- `internal/platform/config.go`: افزودن `HTTPConfig.BodyLimit`، مقدار پیش‌فرض ۱ مگابایت، پارسر `parsePositiveInt` و متغیر محیطی `HTTP_BODY_LIMIT`؛ `internal/platform/app.go` آن را در `fiber.Config.BodyLimit` اعمال می‌کند.

### ۳) آزمون سرتاسری HTTP

- بازنویسی `internal/e2e/e2e_integration_test.go` با schema اختصاصی هر آزمون، اجرای migrationها با `platform.NewMigrator` و seed سیاست کمیسیون نسخه ۲ (نرخ ۱۲۵۰ BPS). چرخه کامل مشتری/ارائه‌دهنده/مدیر از ثبت‌نام و راه‌اندازی نمایه تا رزرو، شارژ، تسویه، اختلاف، تسویه ارائه‌دهنده و بازپرداخت idempotent را پوشش می‌دهد.
- رفع اشکال واقعی مسیریابی: گروه `/api/v1` با middleware احراز هویت مسیرهای عمومی ثبت‌شده بعد از خود (availability و callback پرداخت) را می‌بلعید و خطای `401` می‌داد. مسیر `/api/v1/me` با middleware درون‌خطی ثبت شد و مسیرهای عمومی آزاد ماندند.

### ۴) آزمون هم‌زمانی و صحت مالی

- افزودن `internal/booking/stage09_integrity_test.go` با دو سناریو:
  - تسویه تکمیل: ۸ درخواست هم‌زمان `ConfirmCompletion`؛ فقط یک سند `booking:<id>:settle` و درآمد ارائه‌دهنده دقیقاً ۲۱۸۷۵ (قیمت ۲۵۰۰۰ با کمیسیون ۱۲.۵٪) و دفترکل تراز.
  - بازپرداخت: ۸ درخواست هم‌زمان `Refund`؛ فقط یک سند `booking:<id>:refund-reversal`، بازگشت کامل موجودی مشتری، صفرشدن درآمد ارائه‌دهنده و کمیسیون، تراز دفترکل و موفقیت `Reconcile`.

### ۵) قرارداد و مستندات

- `api/openapi.yaml`: افزودن مسیرهای `auth/register|login|refresh|logout` و `me` به‌همراه schemaهای `AuthRegisterInput`، `AuthLoginInput`، `AuthRefreshInput`، `TokenPair`، `OperationResult` و `User`؛ به‌روزرسانی توضیح چرخش/اثر replay.
- `internal/platform/openapi_stage09_test.go`: آزمون قراردادی برای حضور این مسیرها.
- `README.md`: اصلاح گذارهای ماندگار (حذف endpoint منسوخ `complete`)، افزودن `HTTP_BODY_LIMIT`، بخش سخت‌سازی مرحله 09، فرمان‌های آزمون جدید و ثبت صریح محدودیت `-race`.

## نتیجه اجرای آزمون‌ها

میزبان: Windows؛ پایگاه داده آزمایشی `bookings_test` روی PostgreSQL واقعی (`DATABASE_URL=postgres://booker:***@localhost:5432/bookings_test?sslmode=disable`).

- `gofmt -l .` → بدون خروجی (هیچ فایل قالب‌نشده‌ای نیست).
- `go vet ./...` → بدون خطا (کد خروج ۰).
- `go test ./... -count=1` → همه بسته‌ها موفق:

```text
?   	test-api/cmd/api	[no test files]
ok  	test-api/internal/auth	4.528s
ok  	test-api/internal/booking	65.183s
ok  	test-api/internal/catalog	1.258s
ok  	test-api/internal/e2e	9.007s
ok  	test-api/internal/identity	4.328s
ok  	test-api/internal/payments	23.342s
ok  	test-api/internal/payouts	26.030s
ok  	test-api/internal/platform	6.705s
ok  	test-api/internal/providerapi	5.863s
ok  	test-api/internal/providers	1.144s
ok  	test-api/internal/scheduling	1.125s
ok  	test-api/internal/wallet	1.517s
?   	test-api/migrations	[no test files]
```

- `go test ./internal/e2e -run TestFullBusinessWorkflow -count=1 -v` → همه زیرآزمون‌ها موفق:
  `provider_onboarding`, `availability_is_public_and_descriptive`, `refresh_preserves_identity`, `insufficient_funds_then_topup`, `happy_path_booking_and_payout`, `dispute_settlement`, `admin_refund_is_idempotent`, `financial_integrity`.
- `go test ./internal/booking -run 'TestConcurrentConfirmCompletionSettlesOnce|TestConcurrentRefundReversesOnce'` → هر دو موفق.

## موارد اجرانشده‌نشده یا مسدود

- `go test -race ./...` اجرا **نشده** است؛ detector رقابت به cgo و کامپایلر C نیاز دارد و در این میزبان `gcc` نصب نیست (`CGO_ENABLED=1` با خطای `cgo: C compiler "gcc" not found` شکست می‌خورد). این مورد به‌عنوان اجرانشده ثبت می‌شود، نه موفق. برای اجرا باید تصویر Docker حاوی Go و کامپایلر C به کار رود.
- تماس موفق با Sandbox/Production درگاه Zarinpal انجام نشده است؛ مستندات roundtrip ایجاد نشده و به مجوز/credential واقعی نیاز دارد.

## دروازه‌های باقی‌مانده پیش از عرضه واقعی (بدون تغییر در این مرحله)

- پایش/هشدار، پشتیبان‌گیری و بازیابی، runbook، ظرفیت و بازبینی امنیت مستقل.
- بررسی حقوقی/مقرراتی/مالیاتی/KYC ایران و شرایط ارائه‌دهنده پرداخت.
- بستن اختلاف مستند حداقل مبلغ Zarinpal و قرارداد callback/error/retry با ارائه‌دهنده.
