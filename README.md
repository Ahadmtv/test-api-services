# SAMA — سامانه رزرو خدمات

سامانه رزرو خدمات با نقش‌های مشتری، ارائه‌دهنده خدمت و مدیر.
مرحله‌های 02 و 03 شامل احراز هویت، حساب‌های کاربری و مجوزها، نمایه ارائه‌دهنده، کاتالوگ خدمات و برنامه هفتگی/مرخصی هستند. مرحله 06 چرخه رزرو با تأیید فوری، قفل کیف پول، لغو، آغاز خدمت را اضافه می‌کند؛ شارژ کیف پول از مسیر Zarinpal در Sandbox طبق مرز provider مرحله 07 انجام می‌شود. مرحله 08 (طبق ADR 0013) workflow تکمیل مشتری‌محور را اضافه می‌کند: ارائه‌دهنده درخواست تکمیل می‌دهد (`request-completion`)، مشتری تأیید یا اختلاف ثبت می‌کند، مدیر اختلاف را حل می‌کند و رزروهای خارج پنجره با فرمان عملیاتی `bookings-settle-expired` تسویه می‌شوند. ارائه‌دهنده همچنین می‌تواند با `POST /api/v1/provider/payouts` درخواست تسویه داخلی بدهد که درآمد او را در دفترکل رزرو می‌کند؛ گذارهای پرداخت دستی مدیر با ارجاع انتقال بیرونی ثبت می‌شوند (تسویه بانکی واقعی در دامنه نیست).
طبق ADR 0010، رزرو همچنان فوری `confirmed` است، اما مبلغ صحیح ریال آن در همان تراکنش از کیف پول مشتری قفل می‌شود؛ لغو قفل را آزاد و تأیید تکمیل آن را تسویه می‌کند.

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
| `HTTP_BODY_LIMIT` | حداکثر اندازه بدنه درخواست HTTP به بایت (باید عددی مثبت باشد) | ۱۰۴۸۵۷۶ (۱ مگابایت) |

رازهای حقیقی (مثل `DB_PASSWORD` و `AUTH_ACCESS_TOKEN_SECRET`) هرگز در مخزن ثبت نشود.

## نصب و راه‌اندازی محلی

### ۱) با Docker Compose (توصیه‌شده)

```bash
docker compose up -d db
go run ./cmd/api migrate
go run ./cmd/api
```

API در `http://localhost:8080` در دسترس است.

اجرای API داخل Docker نیز ممکن است؛ پیش از بالا آمدن سرویس، مهاجرت‌ها را روی همان پایگاه‌داده اجرا کنید:

```bash
docker compose up -d db
docker compose run --rm api migrate
docker compose up -d api
```

مهاجرت اجرانشده باعث خطای داخلی ایجاد رزرو می‌شود؛ پس از هر به‌روزرسانی کد، `migrate` را دوباره اجرا کنید.

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

رزرو با روش پیش‌فرض `wallet` بلافاصله با وضعیت `confirmed` ثبت می‌شود و برای تأیید دستی یا TTL پذیرش provider منتظر نمی‌ماند؛ برای خدمت با مبلغ مثبت، موجودی قابل‌استفاده کافی لازم است و مبلغ خدمت قفل می‌شود. خدمت رایگان بدون قفل ثبت می‌شود. با `payment_method=zarinpal` رزرو در وضعیت `pending_payment` با `payment_expires_at` ساخته می‌شود، بازه را نگه می‌دارد و فقط پس از Verify موفق سرور `confirmed` می‌شود؛ مبلغ در این مسیر مستقیماً به قفل رزرو می‌رود و موجودی قابل‌استفاده کیف پول را افزایش نمی‌دهد (ADR 0016). گذارهای فعال طبق ADR 0013 عبارت‌اند از: `confirmed → cancelled` با لغو مشتری پیش از مهلت `BOOKING_CANCELLATION_CUTOFF`، `confirmed → cancelled` با لغو ارائه‌دهنده مالک پیش از آغاز، `confirmed → in_progress` با آغاز خدمت، `in_progress → completion_pending` با درخواست تکمیل، `completion_pending → completed` با تأیید مشتری یا `disputed` با ثبت اختلاف، و `disputed → completed | cancelled` با حل اختلاف مدیر. قفل وجه فقط هنگام لغو آزاد و هنگام تأیید تکمیل/حل اختلاف/انقضای پنجره به درآمد داخلی ارائه‌دهنده و کمیسیون تسویه می‌شود.

`GET /api/v1/wallet` مانده قابل‌استفاده، قفل‌شده و درآمد داخلی ارائه‌دهنده را برمی‌گرداند؛ `GET /api/v1/wallet/transactions` تاریخچه صفحه‌بندی‌شده اسناد دفترکل است. مشتری احرازشده با `POST /api/v1/payments/zarinpal` و `Idempotency-Key` پرداخت جدید ایجاد می‌کند. `purpose=wallet_topup` (پیش‌فرض) مبلغ `amount_irr` را برای شارژ کیف پول می‌گیرد؛ `purpose=booking` فقط `booking_id` را می‌گیرد و مبلغ را از snapshot سرور می‌خواند. پاسخ شامل `redirect_url` و شناسه محلی پرداخت است. شکست تأییدشده پرداخت رزرو را لغو می‌کند و `go run ./cmd/api bookings-expire-payments` رزروهای `pending_payment` بدون تلاش پرداخت فعال را پس از مهلت `BOOKING_PENDING_PAYMENT_TTL` منقضی می‌کند. وضعیت متعلق به مشتری با `GET /api/v1/payments/{paymentID}` خوانده می‌شود. callback عمومی `GET /api/v1/payments/zarinpal/callback` فقط آغازگر بررسی server-to-server است و پارامتر مرورگر مدرک پرداخت نیست. تنها نتیجه تأییدشده با authority و مبلغ مطابق، کیف پول را در همان تراکنش وضعیت `succeeded` شارژ می‌کند. تکرار callback سند ledger دوم نمی‌سازد.

بازپرداخت فقط با نقش مدیر، برای رزرو تکمیل‌شده و با `Idempotency-Key` در `POST /api/v1/admin/bookings/{bookingID}/refund` ممکن است. ثبت بازپرداخت، سند settlement را معکوس می‌کند و مبلغ قفل‌شده را در همان تراکنش به موجودی قابل‌استفاده مشتری برمی‌گرداند؛ این عمل بازپرداخت بانکی/درگاهی نیست.

پرداخت‌ها وضعیت‌های `created`، `pending`، `succeeded`، `failed` و `verification_unknown` دارند. درخواست ایجاد بیرونی هنگام timeout یا پاسخ نامعتبر تکرار خودکار نمی‌شود؛ نتیجه نامطمئن حفظ می‌شود. `go run ./cmd/api payments-reconcile` حداکثر 100 پرداخت نامطمئن دارای authority را با Verify دوباره بررسی می‌کند و شمارش وضعیت‌های حل‌نشده (از جمله پرداخت‌های بدون authority) را در log اپراتوری گزارش می‌کند. پرداختی که هنگام ایجاد authority قابل‌ذخیره ندارد با این فرمان قابل تطبیق خودکار نیست و نیازمند رسیدگی عملیاتی است؛ روش مستند «استعلام تراکنش» Zarinpal فقط با authority کار می‌کند و لیست‌های بیرونی کلید محلی یکتا ندارند، بنابراین بازیابی خودکار برای این رده وجود ندارد و فرمان تطبیق برای آن credit یا خطای قطعی تزریق نمی‌کند.

فقط کد 100 یا 101 همراه `ref_id` غیرخالی نتیجه تأییدشده است؛ در Verify، کد 101 بدون `ref_id` و سایر کدهای مستندنشده به‌عنوان نتیجه ناشناخته حفظ می‌شوند (وجود `ref_id` برای 101 در منابع رسمی تضمین نشده است) و هیچ کد تأییدنشده‌ای خطای قطعی فرض نمی‌شود. مبلغ verify همیشه از رکورد محلی خوانده می‌شود؛ پاسخ v4 نه مبلغ و نه authority را بازمی‌گرداند، پس مقایسه echo داخلی تأیید مستقل درگاه نیست و ظرفیت اجرا آن از آزمون Sandbox خواسته می‌شود. اگر تأیید درگاه موفق شود ولی commit محلی شکست بخورد، همان پرداخت در `verification_unknown` می‌ماند و با تأییدهای بعدی — از جمله callback یا فرمان تطبیق — قابل بازیابی است؛ تأیید تکراری با قید یکتای سند funding بی‌اثر مالی است و بازیابی مبلغ از دریافت دوباره جهت جلوگیری از اعتبار تکراری انجام نمی‌شود.

callback بازگشت مرورگر به `PAYMENT_CALLBACK_URL` پس از Verify سمت سرور با `303 See Other` مرورگر را به `PAYMENT_RETURN_URL` (صفحه نتیجه اپ) هدایت می‌کند. این نشانی ثابت و مطلق است و هیچ authority، شناسه پرداخت، توکن یا مسیر بازگشتی در آن قرار نمی‌گیرد؛ صفحه نتیجه شناسه کوتاه‌عمر را از کوکی httpOnly می‌خواند و وضعیت را از `GET /api/v1/payments/{paymentID}` احراز‌شده می‌گیرد. Zarinpal در callback واقعی پارامترهای `Authority` و `Status` را با حرف بزرگ می‌فرستد؛ handler هر دو شکل بزرگ و کوچک را می‌پذیرد و اعتبارسنجی نهایی همچنان با Verify سمت سرور انجام می‌شود.

`PAYMENT_ENVIRONMENT` به‌طور پیش‌فرض `sandbox` است و فقط مقدار صریح `production` محیط تولید را انتخاب می‌کند. در sandbox خالی‌بودن `ZARINPAL_MERCHANT_ID` با یک UUID غیرراز ثابت جبران می‌شود، مطابق مستند رسمی sandbox که merchant_id را یک UUID دلخواه می‌داند؛ در production این مقدار الزامی است و مقدار نامعتبر یا خالی مانع شروع سرویس می‌شود. `PAYMENT_CALLBACK_URL` و `PAYMENT_RETURN_URL` از محیط تأمین می‌شوند؛ `PAYMENT_RETURN_URL` در production الزامی است. راز merchant در لاگ/کد ثبت نمی‌شود. مبلغ دامنه و مبلغ ارسالی به Zarinpal هر دو عدد صحیح IRR هستند و تبدیل واحد انجام نمی‌شود. حداقل 10,000 ریال در صفحه رسمی درخواست پرداخت صریحاً بیان شده است و پیاده‌سازی همان حد را محافظه‌کارانه اعمال می‌کند؛ اختلاف اعلام‌شده در validator رسمی SDK (1,000) به‌عنوان یک سؤال باز پیش از عرضه با Zarinpal بسته می‌شود.

مستند رسمی بررسی‌شده در 2026-10-19: [درخواست پرداخت](https://www.zarinpal.com/docs/sdk/nodejs/method/request.html)، [تأیید پرداخت](https://www.zarinpal.com/docs/sdk/nodejs/method/verify.html)، [استعلام تراکنش](https://www.zarinpal.com/docs/sdk/nodejs/method/inquiry.html)، [پیکربندی SDK](https://www.zarinpal.com/docs/sdk/nodejs/configuration.html)، [SDK رسمی](https://github.com/ZarinPal/ZarinPal-node-SDK). مستندات wire-level پاسخ کامل create، callback و فهرست خطاها را تعریف نمی‌کنند؛ استعلام/فهرست‌های مستند در این مرحله پیاده‌سازی نشده‌اند. در آزمون واقعی Sandbox در 2026-10-20، ایجاد پرداخت 20,000 ریالی با authority معتبر، باز شدن `/pg/StartPay/`، تکمیل پرداخت، callback با هدایت 303 به صفحه نتیجه، Verify با کد 100، بستانکارشدن یک‌باره کیف پول و نمایش موجودی به‌روز در حساب کاربری تأیید شد. Production و استفاده از merchant واقعی در این تغییر فراخوانی نشده‌اند.

#### مدل حسابداری و محدودیت‌ها

- هر سند ledger دست‌کم دو ثبت دارد و جمع بدهکار و بستانکار آن برابر است. حساب‌های مشتری برای موجودی قابل‌استفاده و قفل‌شده جدا هستند؛ حساب‌های ارائه‌دهنده درآمد داخلی، کمیسیون پلتفرم، clearing تأمین اعتبار و clearing بازپرداخت نیز به‌صورت حساب‌های دفترکل نگهداری می‌شوند.
- `wallet_accounts.balance_irr` فقط projection مانده است؛ ثبت‌های تغییرناپذیر `ledger_entries` منبع حقیقت‌اند. trigger پایگاه‌داده projection را در همان تراکنش ثبت دفترکل به‌روز می‌کند، موجودی منفی و تغییر مستقیم حساب را منع می‌کند، و `wallet-reconcile` اختلاف projection، تراز سندها و پیوند قفل/رزرو را گزارش می‌دهد.
- درخواست ایجاد رزرو، hold و سند دوطرفه را اتمی ثبت می‌کند؛ لغو پیش از cutoff قفل را آزاد می‌کند؛ تکمیل خدمت قفل را با نرخ کمیسیون snapshot شده به درآمد ارائه‌دهنده و کمیسیون تقسیم می‌کند. رزرو رایگان بدون hold ثبت می‌شود. سیاست اولیه migration نرخ صفر BPS و گردکردن floor دارد؛ نرخ محصول/قرارداد مالی واقعی هنوز تعیین نشده است.
- بازپرداخت فقط یک‌بار و با سند جبرانی انجام می‌شود؛ تاریخچه قبلی و سند settlement حذف یا ویرایش نمی‌شوند. قفل رزرو منقضی‌شده/worker expiry وجود ندارد، چون رزرو فوری `confirmed` است؛ مهلت لغو رزرو سیاست فعلی آزادسازی وجه است.
- شارژ کیف پول از طریق adapter Zarinpal پیاده‌سازی شده، اما تأیید عملی Sandbox انجام نشده و Production تا رفع ابهام‌های مستند، تأیید واحد/محدوده مبلغ و بررسی حقوقی/مقرراتی/مالیاتی/KYC فعال یا تأییدشده محسوب نمی‌شود. برداشت/تسویه بانکی و بازپرداخت خارجی وجود ندارد. `FundForDevelopment` همچنان endpoint عمومی ندارد.

### ۹) بازگردانی migration (اختیاری)

```bash
go run ./cmd/api migrate-down
```

## سخت‌سازی و راستی‌آزمایی (مرحله 09)

مرحله 09 رفتار موجود را بازنویسی نمی‌کند؛ صحت آن را روی PostgreSQL واقعی و مسیر HTTP می‌سنجد و شکاف‌های امنیتی/هم‌زمانی را می‌بندد:

- **چرخش اتمی توکن Refresh:** تایید مجدد توکن را در یک تراکنش قفل، مصرف، لغو و جایگزین می‌کند. توکن دسترسی تازه ایمیل و نقش‌های جاری کاربر را حفظ می‌کند (پیش‌تر نقش‌ها در چرخش از دست می‌رفت). استفاده مجدد از توکن چرخیده‌شده به‌عنوان replay شناسایی و همه نشست‌های کاربر لغو می‌شود.
- **محدودیت نرخ ایجاد پرداخت:** مسیر `POST /api/v1/payments/zarinpal` با همان سقف `AUTH_RATE_LIMIT_PER_MIN` و کد خطای `rate_limited` (پاسخ `429`) محافظت می‌شود.
- **محدودسازی اندازه بدنه:** همه مسیرها با `HTTP_BODY_LIMIT` (پیش‌فرض ۱ مگابایت) در Fiber محدود می‌شوند.
- **تضمین هم‌زمانی مالی:** تأیید تکمیل هم‌زمان فقط یک بار تسویه می‌کند و بازپرداخت هم‌زمان فقط یک بار معکوس می‌شود؛ اثر دفترکل و تغییر وضعیت در یک تراکنش‌اند و قید exclusion پایگاه داده هم‌پوشانی رزرو را در رقابت رد می‌کند.
- **مسیر HTTP سرتاسری:** آزمون `internal/e2e` چرخه واقعی مشتری/ارائه‌دهنده/مدیر را از ثبت‌نام و راه‌اندازی نمایه تا رزرو، شارژ، تسویه، اختلاف، تسویه ارائه‌دهنده و بازپرداخت idempotent روی Fiber واقعی و PostgreSQL اجرا می‌کند.

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

تست‌های یکپارچه migration، حساب کاربری، API مرحله 03، چرخه رزرو و دفترکل به PostgreSQL واقعی و متغیر `DATABASE_URL` نیاز دارند و در غیر این صورت رد می‌شوند (skip). برای بررسی projection، تراز سندها و پیوند قفل‌ها در محیط مهاجرت‌شده، `go run ./cmd/api wallet-reconcile` را اجرا کنید.

```bash
go test ./... -run 'Test(ServiceIntegration|Stage03APIIntegration|MigrationsIntegration)' -v
go test ./internal/booking -run 'TestConcurrentOverlappingBookingRequests|TestDatabaseExclusionConstraintRejectsOverlap|TestInstantBookingMigrationCancelsExistingPendingRows' -count=1 -v
go test ./internal/booking -run 'TestConcurrentConfirmCompletionSettlesOnce|TestConcurrentRefundReversesOnce' -count=1 -v
go test ./internal/e2e -run TestFullBusinessWorkflow -count=1 -v
```

آزمون هم‌زمانی هر بار ده درخواست overlapping را روی PostgreSQL واقعی اجرا می‌کند و در خود آزمون سه بار تکرار می‌شود. آزمون‌های هم‌زمانی مالی مرحله 09 ضمناً ثابت می‌کنند تسویه و معکوس‌سازی هر کدام دقیقاً یک سند ledger می‌سازند و دفترکل پس از رقابت تراز می‌ماند.

اجرای `go test -race` روی میزبان Windows این مخزن انجام نشده است، چون detector رقابت به cgo و یک کامپایلر C (مثل `gcc`) نیاز دارد که در این محیط نصب نیست؛ این مورد به‌عنوان مورد اجرانشده ثبت می‌شود و نه موفق. برای اجرای آن باید تصویر Docker حاوی Go و کامپایلر C استفاده شود.

## ساخت تصویر Docker

```bash
docker build -t test-api:0.2.0 .
```

## محدودیت‌های حقوقی

کیف پول داخلی فقط دفتر حسابداری است و مجوز نگهداری وجه مشتری یا تسویه بانکی ایجاد نمی‌کند. تأیید حقوقی، مالیاتی، احراز هویت و شرایط ارائه‌دهنده پرداخت پیش از عرضه واقعی الزامی است. اتصال Zarinpal به‌تنهایی اثبات پرداخت واقعی یا مجوز نگهداری وجه نیست. در مرحله 08، ثبت «پرداخت‌شده» فقط گواهی مدیرِ اپراتوری بر یک انتقال بانکی بیرونی است و خدمت تسویه بانکی به ارائه‌دهنده پیاده‌سازی نمی‌کند و جایگزین آن نمی‌شود.

## مراحل آینده

رزروها snapshot مستقل نام، مدت، قیمت IRR و بافر خدمت را نگه می‌دارند؛ ویرایش خدمت، snapshot تاریخی را تغییر نمی‌دهد. مرحله 08 workflow تکمیل/اختلاف و رزرو تسویه ارائه‌دهنده را اضافه کرد؛ no-show مؤثر، بازپرداخت جزئی و انتقال بانکی واقعی همچنان خارج از دامنه‌اند و به تصمیم معلق آینده سپرده شده‌اند.
