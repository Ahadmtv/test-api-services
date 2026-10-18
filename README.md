# SAMA — سامانه رزرو خدمات

سامانه رزرو خدمات با نقش‌های مشتری، ارائه‌دهنده خدمت و مدیر.
در این مرحله (مرحله 01) فقط زیرساخت پروژه پیاده شده است و منطق رزرو، کیف پول و پرداخت هنوز اضافه نشده است.

## پیش‌نیازها

- Go 1.27 یا بالاتر
- PostgreSQL 16+ (یا Docker Compose برای محیط محلی)

## نصب و راه‌اندازی محلی

### ۱) تنظیم متغیرهای محیطی

```bash
cp .env.example .env
# مقادیر .env را مطابق محیط محلی تغییر دهید
```

رازهای حقيقي (مثل `DB_PASSWORD`) هرگز در مخزن ثبت نشود.

### ۲) با Docker Compose (توصیه‌شده)

```bash
docker compose up -d db
go run ./cmd/api migrate
go run ./cmd/api
```

API در `http://localhost:8080` در دسترس است.

### ۳) بدون Docker (PostgreSQL محلی)

پایگاه داده `bookings` و کاربر `booker` را بسازید، سپس:

```bash
export $(cat .env | xargs)
go run ./cmd/api migrate
go run ./cmd/api
```

### ۴) بررسی سلامت

```bash
curl http://localhost:8080/health/live   # زنده بودن فرآیند (بدون وابستگی به پایگاه داده)
curl http://localhost:8080/health/ready  # آمادگی سرویس (نیاز به پایگاه داده)
```

### ۵) بازگردانی migration (اختیاری)

```bash
go run ./cmd/api migrate-down
```

## اجرای تست‌ها

```bash
go test ./...
```

تست‌های واحد روی متغیر محیطی اجرا می‌شوند. تست‌های یکپارچه migration نیاز به متغیر
`DATABASE_URL` دارند و در غیر این صورت رد می‌شوند (skip).

## ساخت تصویر Docker

```bash
docker build -t test-api:0.1.0 .
```

## محدودیت‌های حقوقی

کیفلت داخلی فقط دفتر حسابداری است و مجوز نگهداری وجه مشتری یا تسویه بانکی ایجاد نمی‌کند.
تأیید حقوقی، مالیاتی، احراز هویت و شرایط ارائه‌دهنده پرداخت (Zarinpal Sandbox) پیش از عرضه واقعی الزامی است.
