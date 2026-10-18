# SAMA — سامانه رزرو خدمات

سامانه رزرو خدمات با نقش‌های مشتری، ارائه‌دهنده خدمت و مدیر.
مرحله‌های 02 و 03 شامل احراز هویت، حساب‌های کاربری و مجوزها، نمایه ارائه‌دهنده، کاتالوگ خدمات و برنامه هفتگی/مرخصی هستند.
ایجاد رزرو، کیف پول و پرداخت در مراحل آینده اضافه می‌شوند.

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

رازهای حقيقي (مثل `DB_PASSWORD` و `AUTH_ACCESS_TOKEN_SECRET`) هرگز در مخزن ثبت نشود.

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

تست‌های یکپارچه migration، حساب کاربری و API مرحله 03 به PostgreSQL و متغیر `DATABASE_URL` نیاز دارند و در غیر این صورت رد می‌شوند (skip).

```bash
go test ./... -run 'Test(ServiceIntegration|Stage03APIIntegration|MigrationsIntegration)' -v   # نیاز به DATABASE_URL
```

## ساخت تصویر Docker

```bash
docker build -t test-api:0.2.0 .
```

## محدودیت‌های حقوقی

کیفلت داخلی فقط دفتر حسابداری است و مجوز نگهداری وجه مشتری یا تسویه بانکی ایجاد نمی‌کند.
تأیید حقوقی، مالیاتی، احراز هویت و شرایط ارائه‌دهنده پرداخت (Zarinpal Sandbox) پیش از عرضه واقعی الزامی است.

## مراحل آینده

ایجاد رزرو، snapshot تاریخی نام/مدت/قیمت/ارز و سیاست buffer، کیف پول، دفترکل، تسویه و پرداخت هنوز پیاده‌سازی نشده‌اند. هنگام ایجاد رزرو باید مقادیر توافق‌شده خدمت در snapshot مستقل رزرو ثبت شوند؛ ویرایش خدمت نباید snapshot رزرو را تغییر دهد.
