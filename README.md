# SAMA — سامانه رزرو خدمات

سامانه رزرو خدمات با نقش‌های مشتری، ارائه‌دهنده خدمت و مدیر.
مرحله‌های 02 و 03 شامل احراز هویت، حساب‌های کاربری و مجوزها، نمایه ارائه‌دهنده، کاتالوگ خدمات و برنامه هفتگی/مرخصی هستند. چرخه رزرو مرحله 05 نیز با تأیید فوری، لغو و آغاز خدمت پیاده‌سازی شده است.
طبق ADR 0009، این چرخه رزرو هیچ وجهی را دریافت یا قفل نمی‌کند؛ کیف پول، پرداخت و تسویه خارج از این پیاده‌سازی هستند.

## پیش‌نیازها

- Go 1.27 یا بالاتر
- PostgreSQL 16+ (یا Docker Compose برای محیط محلی)

## متغیرهای محیطی

```bash
cp .env.example .env
# مقادیر .env را مطابق محیط محلی تغییر دهید
```

متغیرهای جدید مرحله 02:

| متغیر | توضیح | پیش‌فرض |
|--------|--------|---------|
| `AUTH_ACCESS_TOKEN_SECRET` | کلید امضای توکن دسترسی JWT (حداقل ۳۲ کاراکتر، الزامی) | — |
| `AUTH_ACCESS_TOKEN_TTL` | عمر توکن دسترسی | ۱۵ دقیقه |
| `AUTH_REFRESH_TOKEN_TTL` | عمر توکنRefresh | ۱۶۸ ساعت |
| `AUTH_RATE_LIMIT_PER_MIN` | محدودیت نرخ برای مسیرهای احراز هویت (در هر دقیقه، بر اساس IP) | ۳۰ |
| `BOOKING_CANCELLATION_CUTOFF` | فاصله لازم پیش از آغاز خدمت برای لغو رزرو تأییدشده | صفر |

رازهای حقیقی (مثل `DB_PASSWORD` و `AUTH_ACCESS_TOKEN_SECRET`) هرگز در مخزن ثبت نشود.

## نصب و راه‌اندازی محلی

### ۱) با Docker Compose (توصیه‌شده)

```bash
docker compose up -d db
go run ./cmd/api migrate
go run ./cmd/api
```

API در `http://localhost:8080` در دسترس است.

### ۲) بدون Docker (PostgreSQL محلی)

پایگاه داده `bookings` و کاربر `booker` را بسازید، سپس:

```bash
export $(cat .env | xargs)
go run ./cmd/api migrate
go run ./cmd/api
```

### ۳) بررسی سلامت

```bash
curl http://localhost:8080/health/live   # زنده بودن فرآیند (بدون وابستگی به پایگاه داده)
curl http://localhost:8080/health/ready  # آمادگی سرویس (نیاز به پایگاه داده)
```

### ۴) مسیرهای احراز هویت (مرحله 02)

```bash
# ثبت نام
curl -X POST http://localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"user@example.com","phone":"+989120000000","password":"secret1234"}'

# ورود (دریافت access_token و refresh_token)
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"user@example.com","password":"secret1234"}'

# تایید مجدد توکن (rotate)
curl -X POST http://localhost:8080/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d '{"refresh_token":"..."}'

# خروج
curl -X POST http://localhost:8080/api/v1/auth/logout \
  -H 'Content-Type: application/json' \
  -d '{"refresh_token":"..."}'

# پروفایل (نیاز به توکن دسترسی در هدر Authorization)
curl -X GET http://localhost:8080/api/v1/me \
  -H 'Authorization: Bearer <access_token>'
```

### ۵) مسیرهای ارائه‌دهنده (مرحله 03)

مسیرهای مدیریتی ارائه‌دهنده به access token با نقش `provider` نیاز دارند. نقش ارائه‌دهنده در ثبت‌نام عمومی صادر نمی‌شود و باید از فرایند تأمین دسترسی تحت کنترل مدیر اعطا شود؛ این مرحله endpoint اعطای نقش اضافه نمی‌کند. نمایه تا زمان تأیید مدیر در کاتالوگ عمومی نمایش داده نمی‌شود.

```bash
# ایجاد یا به‌روزرسانی نمایه؛ منطقه زمانی پیش‌فرض Asia/Tehran است
curl -X PUT http://localhost:8080/api/v1/provider/profile \
  -H 'Authorization: Bearer <access_token>' \
  -H 'Content-Type: application/json' \
  -d '{"display_name":"مرکز خدمات","description":"توضیح","time_zone":"Asia/Tehran","is_active":true}'

# ثبت خدمت با مبلغ صحیح ریال
curl -X POST http://localhost:8080/api/v1/provider/services \
  -H 'Authorization: Bearer <access_token>' \
  -H 'Content-Type: application/json' \
  -d '{"name":"مشاوره","description":"جلسه","duration_minutes":45,"price_irr":2500000,"buffer_minutes":10,"is_active":true}'

# دو بازه در یک روز؛ روز ISO-8601 است (دوشنبه 1 تا یکشنبه 7)
curl -X PUT http://localhost:8080/api/v1/provider/working-hours \
  -H 'Authorization: Bearer <access_token>' \
  -H 'Content-Type: application/json' \
  -d '{"intervals":[{"weekday":1,"opens_at":"09:00","closes_at":"12:00"},{"weekday":1,"opens_at":"13:00","closes_at":"17:00"}]}'

# ثبت بازه مسدود با زمان RFC 3339 دارای offset
curl -X POST http://localhost:8080/api/v1/provider/time-off \
  -H 'Authorization: Bearer <access_token>' \
  -H 'Content-Type: application/json' \
  -d '{"kind":"blocked","starts_at":"2030-03-20T09:00:00+03:30","ends_at":"2030-03-20T10:00:00+03:30","reason":"عدم حضور"}'
```

ساعات هفتگی زمان دیواری محلی‌اند، نه UTC. بازه‌های ساعات کاری نیمه‌باز `[شروع، پایان)`، در یک روز و غیرهم‌پوشان هستند؛ بازه عبوری از نیمه‌شب باید به دو روز جدا شکسته شود. تغییر منطقه زمانی، ساعت‌های دیواری موجود را در منطقه تازه تفسیر می‌کند و آن‌ها را به زمان دیگری تبدیل نمی‌کند. مرخصی/مسدودی با ابتدا و انتهای لحظه‌ای در `TIMESTAMPTZ` و به UTC ذخیره می‌شود. قرارداد کامل در `api/openapi.yaml` است.

### ۷) بررسی در دسترس بودن (مرحله 03)

```bash
# در دسترس بودن برای ارائه‌دهنده و خدمت در تاریخ مشخص (تاریخ میلادی در منطقه زمانی ارائه‌دهنده)
curl -X GET "http://localhost:8080/api/v1/providers/1/availability?service_id=1&date=2025-03-15" \
  -H 'Authorization: Bearer <access_token>'
```

پاسخ:
```json
{
  "provider_id": 1,
  "service_id": 1,
  "date": "2025-03-15",
  "time_zone": "Asia/Tehran",
  "slots": [
    {"start_at": "2025-03-15T05:30:00Z", "end_at": "2025-03-15T06:30:00Z"},
    {"start_at": "2025-03-15T06:00:00Z", "end_at": "2025-03-15T07:00:00Z"}
  ]
}
```

- پارامتر `date` تاریخ تقویمی میلادی در منطقه زمانی محلی ارائه‌دهنده است (مثال: `2025-03-15`).
- بازه‌های بازگشتی بر اساس ساعت کاری، مرخصی، رزروهای موجود و بافر محاسبه شده‌اند.
- **نتیجه مشورتی است و تضمین رزرو محسوب نمی‌شود.**
- بازه‌های بازگشتی بر اساس بازه‌های نیمه‌باز `[شروع، پایان)` و به فرمت ISO 8601 (UTC) برگردانده می‌شوند.
- بافر (`buffer_minutes`) به پایان خدمت اضافه می‌شود و باید در ساعت کاری بماند.
- اگر область زمانی ارائه‌دهنده باعث زمان مبهم/ناموجود شود (مثل تغییر ساعت تابستانی)، بازه‌های مربوطه حذف می‌شوند.

### ۸) چرخه رزرو (مرحله 05)

ایجاد رزرو فقط `service_id` و `starts_at` می‌پذیرد. قیمت، مدت، بافر، پایان و ارائه‌دهنده از تنظیم معتبر سرور گرفته می‌شوند. بازه نیمه‌باز `[starts_at, ends_at + buffer)` است؛ قید exclusion پایگاه داده هم‌پوشانی ارائه‌دهنده را حتی در رقابت هم‌زمان رد می‌کند. `Idempotency-Key` اختیاری است؛ تکرار همان درخواست پاسخ قبلی را می‌دهد و payload متفاوت با همان کلید `409` می‌گیرد.

```bash
# مشتری: ایجاد رزرو
curl -X POST http://localhost:8080/api/v1/bookings \\
  -H 'Authorization: Bearer <client_access_token>' \\
  -H 'Content-Type: application/json' \\
  -H 'Idempotency-Key: booking-20250315-001' \\
  -d '{"service_id":1,"starts_at":"2025-03-15T09:00:00+03:30"}'

# تاریخچه مشتری
curl http://localhost:8080/api/v1/bookings \\
  -H 'Authorization: Bearer <client_access_token>'

# فهرست رزروهای ارائه‌دهنده
curl http://localhost:8080/api/v1/provider/bookings \\
  -H 'Authorization: Bearer <provider_access_token>'

# آغاز خدمت در زمان برنامه‌ریزی‌شده
curl -X POST http://localhost:8080/api/v1/provider/bookings/1/start \\
  -H 'Authorization: Bearer <provider_access_token>'
```

رزرو موفق بلافاصله با وضعیت `confirmed` ثبت می‌شود و برای تأیید دستی یا TTL پذیرش provider منتظر نمی‌ماند. گذار فعال مرحله 05 برابر `confirmed → in_progress | cancelled` است. مشتری رزرو را فقط پیش از مهلت قابل‌تنظیم `BOOKING_CANCELLATION_CUTOFF` لغو می‌کند. هر گذار در جدول تغییرناپذیر `booking_state_transitions` ثبت می‌شود و لغو بازه را آزاد می‌کند.

تکمیل خدمت، اختلاف، کیف پول، پرداخت و تسویه در این مرحله نیستند؛ `completed` فقط برای مراحل بعدی محفوظ است. تأیید فوری رزرو به معنی دریافت وجه نیست.

### ۶) بازگردانی migration (اختیاری)

```bash
go run ./cmd/api migrate-down
```

## امنیت (مرحله‌های 02 و 03)

- رمز عبور‌ها با Argon2id هش شده و متنخام رمز عبور در پایگاه داده ذخیره نمی‌شود.
- توکنRefreshها هش شده در جدول `refresh_tokens` ذخیره می‌شوند؛ plaintext هرگز ثبت نمی‌شود.
- توکن دسترسی به صورت JWT امضا شده است؛ کلید امضا از `AUTH_ACCESS_TOKEN_SECRET` خوانده می‌شود.
- rotate توکنRefresh: هر بار تایید مجدد، توکن قبلی لغو و توکن جدید صادر می‌شود.
- در صورت شناسایی بازیابی (replay) توکنReply، کلیه نشست‌های کاربر لغو می‌شوند.
- پاسخ‌های خطای ورود/ثبت نام یکسان هستند تا از شمارش حساب (account enumeration) جلوگیری شود.
- مسیرهای احراز هویت محدودیت نرخ (rate limit) دارند.
- پاسخ‌های خطا به فرمت `application/problem+json` و شامل `request_id` هستند.
- تمام رمزها و توکن‌ها در لاگ ثبت نمی‌شوند.

## اجرای تست‌ها

```bash
go test ./...
```

تست‌های یکپارچه migration، حساب کاربری، API مرحله 03 و چرخه رزرو به PostgreSQL و متغیر `DATABASE_URL` نیاز دارند و در غیر این صورت رد می‌شوند (skip).

```bash
go test ./... -run 'Test(ServiceIntegration|Stage03APIIntegration|MigrationsIntegration)' -v
go test ./internal/booking -run 'TestConcurrentOverlappingBookingRequests|TestDatabaseExclusionConstraintRejectsOverlap|TestInstantBookingMigrationCancelsExistingPendingRows' -count=1 -v
```

آزمون هم‌زمانی هر بار ده درخواست overlapping را روی PostgreSQL واقعی اجرا می‌کند و در خود آزمون سه بار تکرار می‌شود.

## ساخت تصویر Docker

```bash
docker build -t test-api:0.2.0 .
```

## محدودیت‌های حقوقی

کیفلت داخلی فقط دفتر حسابداری است و مجوز نگهداری وجه مشتری یا تسویه بانکی ایجاد نمی‌کند.
تأیید حقوقی، مالیاتی، احراز هویت و شرایط ارائه‌دهنده پرداخت (Zarinpal Sandbox) پیش از عرضه واقعی الزامی است.

## مراحل آینده

رزروها snapshot مستقل نام، مدت، قیمت IRR و بافر خدمت را نگه می‌دارند؛ ویرایش خدمت، snapshot تاریخی را تغییر نمی‌دهد. مرحله 04 کیف پول/پرداخت و مرحله 06 تکمیل/تسویه هنوز پیاده‌سازی نشده‌اند.
