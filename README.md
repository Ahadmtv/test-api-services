# SAMA — سامانه رزرو خدمات

سامانه رزرو خدمات با نقش‌های مشتری، ارائه‌دهنده خدمت و مدیر.
در این مرحله (مرحله 02) احراز هویت، حساب‌های کاربری و مجوزها پیاده شده‌اند.
منطق رزرو، کیف پول و پرداخت در مراحل آینده اضافه می‌شود.

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

### ۵) بازگردانی migration (اختیاری)

```bash
go run ./cmd/api migrate-down
```

## امنیت (مرحله 02)

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

تست‌های واحد روی متغیر محیطی اجرا می‌شوند. تست‌های یکپارچه migration و حساب کاربری نیاز به متغیر
`DATABASE_URL` دارند و در غیر این صورت رد می‌شوند (skip).

```bash
go test ./... -run TestServiceIntegration -v   # نیاز به DATABASE_URL
```

## ساخت تصویر Docker

```bash
docker build -t test-api:0.2.0 .
```

## محدودیت‌های حقوقی

کیفلت داخلی فقط دفتر حسابداری است و مجوز نگهداری وجه مشتری یا تسویه بانکی ایجاد نمی‌کند.
تأیید حقوقی، مالیاتی، احراز هویت و شرایط ارائه‌دهنده پرداخت (Zarinpal Sandbox) پیش از عرضه واقعی الزامی است.

## esperanza مراحل آینده

رزرو، قفل وجه، کیف پول، استیمنٹ و پرداخت (مراحل ۰۳ تا ۰۶) در این مرحله پیاده‌سازی نشده‌اند.
