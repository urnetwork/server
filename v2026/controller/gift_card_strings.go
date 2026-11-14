package controller

// The words of the gift cards and the gift emails, in the six languages ur.io
// ships (GIFTOFPRIVACY.md §2.5): the buyer's site language picks them, Arabic
// reads right to left, and the buyer's own note and signature stay as written.
// The terms follow ur.io's gift catalogs (react/src/i18n/<lang>.json, "gift"),
// so a card says what the site and the apps say: es "Cuenta › Códigos de
// saldo", de "Konto › Guthabencodes", zh "账户 › 余额码", ru "Аккаунт › Коды
// баланса", ar "الحساب › رموز الرصيد". The legal footer of the emails stays
// English, as for every account email (EMAIL1.md §7).
//
// Every key has all six languages (TestGiftCardStringsHaveEveryLanguage), so a
// missing translation fails the build instead of falling back to English.

import (
	"fmt"
	"strings"
	"time"
)

// The languages of the cards and the emails, the first the default.
var giftCardLanguages = []string{"en", "es", "de", "zh", "ru", "ar"}

// One of the six languages for an order's language ("es", "zh-CN"), English
// for any other.
func giftCardLanguage(language string) string {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(language)), "-")
	for _, supported := range giftCardLanguages {
		if base == supported {
			return supported
		}
	}
	return giftCardLanguages[0]
}

// The direction of the language's text.
func giftCardDir(language string) string {
	if giftCardLanguage(language) == "ar" {
		return "rtl"
	}
	return "ltr"
}

// The strings by key, then language. Keys ending in .one and .other are the
// forms for one gift and for more than one; the plural forms carry no number,
// so Russian and Arabic need no more than these two.
var giftCardStrings = map[string]map[string]string{
	// the card
	"card.title": {
		"en": "URnetwork gift card",
		"es": "Tarjeta de regalo de URnetwork",
		"de": "URnetwork-Geschenkkarte",
		"zh": "URnetwork 礼品卡",
		"ru": "Подарочная открытка URnetwork",
		"ar": "بطاقة هدية من URnetwork",
	},
	"card.kicker": {
		"en": "A gift of privacy for you",
		"es": "Un regalo de privacidad para ti",
		"de": "Ein Geschenk der Privatsphäre für dich",
		"zh": "送您一份隐私之礼",
		"ru": "Приватность в подарок для вас",
		"ar": "هدية الخصوصية لك",
	},
	"card.gift_label": {
		"en": "Your gift",
		"es": "Tu regalo",
		"de": "Dein Geschenk",
		"zh": "您的礼物",
		"ru": "Ваш подарок",
		"ar": "هديتك",
	},
	"card.gift_value": {
		"en": "One year of URnetwork Pro",
		"es": "Un año de URnetwork Pro",
		"de": "Ein Jahr URnetwork Pro",
		"zh": "一年 URnetwork Pro",
		"ru": "Год URnetwork Pro",
		"ar": "عام من URnetwork Pro",
	},
	"card.code_label": {
		"en": "Code",
		"es": "Código",
		"de": "Code",
		"zh": "礼品码",
		"ru": "Код",
		"ar": "الرمز",
	},
	"card.qr_alt": {
		"en": "QR code: redeem this gift at ur.io/redeem",
		"es": "Código QR: canjea este regalo en ur.io/redeem",
		"de": "QR-Code: Löse dieses Geschenk unter ur.io/redeem ein",
		"zh": "二维码：在 ur.io/redeem 兑换这份礼物",
		"ru": "QR-код: активируйте подарок на ur.io/redeem",
		"ar": "رمز QR: استرد هذه الهدية على ur.io/redeem",
	},
	"card.step1": {
		"en": "Get URnetwork at ur.io/install, on any device.",
		"es": "Descarga URnetwork en ur.io/install, en cualquier dispositivo.",
		"de": "Hol dir URnetwork unter ur.io/install, auf jedem Gerät.",
		"zh": "前往 ur.io/install，在任意设备上获取 URnetwork。",
		"ru": "Установите URnetwork с ur.io/install на любое устройство.",
		"ar": "احصل على URnetwork من ur.io/install على أي جهاز.",
	},
	"card.step2": {
		"en": "Enter the code when you sign in, or later under Account, Balance codes.",
		"es": "Introduce el código al iniciar sesión o, más adelante, en Cuenta › Códigos de saldo.",
		"de": "Gib den Code bei der Anmeldung ein oder später unter Konto › Guthabencodes.",
		"zh": "登录时输入礼品码，或稍后在“账户 › 余额码”中输入。",
		"ru": "Введите код при входе или позже в разделе «Аккаунт › Коды баланса».",
		"ar": "أدخل الرمز عند تسجيل الدخول، أو لاحقًا ضمن «الحساب › رموز الرصيد».",
	},
	"card.step3": {
		"en": "Enjoy a year of Pro. The year starts when you redeem.",
		"es": "Disfruta de un año de Pro. El año empieza cuando lo canjeas.",
		"de": "Genieß ein Jahr Pro. Das Jahr beginnt, wenn du den Code einlöst.",
		"zh": "尽享一年 Pro，从兑换之日起计算。",
		"ru": "Пользуйтесь Pro целый год. Год начинается с момента активации.",
		"ar": "استمتع بعام من Pro. يبدأ العام عند الاسترداد.",
	},
	"card.redeem_by": {
		"en": "Redeem by {date}",
		"es": "Canjéalo hasta el {date}",
		"de": "Einlösbar bis {date}",
		"zh": "兑换截止日期：{date}",
		"ru": "Активируйте до {date}",
		"ar": "استرده حتى {date}",
	},
	"card.gift_n": {
		"en": "Gift {n} of {count}",
		"es": "Regalo {n} de {count}",
		"de": "Geschenk {n} von {count}",
		"zh": "第 {n} 份，共 {count} 份",
		"ru": "Подарок {n} из {count}",
		"ar": "الهدية {n} من {count}",
	},
	"card.tagline": {
		"en": "The Privacy Network",
		"es": "La red de privacidad",
		"de": "Das Privatsphäre-Netzwerk",
		"zh": "隐私网络",
		"ru": "Сеть приватности",
		"ar": "شبكة الخصوصية",
	},
	"card.bittensor": {
		"en": "Made with Bittensor",
		"es": "Hecho con Bittensor",
		"de": "Gemacht mit Bittensor",
		"zh": "基于 Bittensor 打造",
		"ru": "Сделано на Bittensor",
		"ar": "صُنع باستخدام Bittensor",
	},

	// the buyer's email (gift_cards_buyer)
	"buyer.subject.one": {
		"en": "Your URnetwork gift card",
		"es": "Tu tarjeta de regalo de URnetwork",
		"de": "Deine URnetwork-Geschenkkarte",
		"zh": "您的 URnetwork 礼品卡",
		"ru": "Ваша подарочная открытка URnetwork",
		"ar": "بطاقة هديتك من URnetwork",
	},
	"buyer.subject.other": {
		"en": "Your URnetwork gift cards",
		"es": "Tus tarjetas de regalo de URnetwork",
		"de": "Deine URnetwork-Geschenkkarten",
		"zh": "您的 URnetwork 礼品卡",
		"ru": "Ваши подарочные открытки URnetwork",
		"ar": "بطاقات هداياك من URnetwork",
	},
	"buyer.preheader.one": {
		"en": "Your gift card is attached as a PDF, ready to print or forward.",
		"es": "Tu tarjeta de regalo va adjunta en PDF, lista para imprimir o reenviar.",
		"de": "Deine Geschenkkarte hängt als PDF an, bereit zum Drucken oder Weiterleiten.",
		"zh": "您的礼品卡已作为 PDF 附上，可直接打印或转发。",
		"ru": "Подарочная открытка во вложении в формате PDF: её можно распечатать или переслать.",
		"ar": "بطاقة هديتك مرفقة بصيغة PDF، جاهزة للطباعة أو إعادة التوجيه.",
	},
	"buyer.preheader.other": {
		"en": "Your gift cards are attached as one PDF, a card per page, ready to print or forward.",
		"es": "Tus tarjetas de regalo van adjuntas en un solo PDF, una por página, listas para imprimir o reenviar.",
		"de": "Deine Geschenkkarten hängen als ein PDF an, eine Karte pro Seite, bereit zum Drucken oder Weiterleiten.",
		"zh": "您的礼品卡已合并为一个 PDF 附上（每页一张），可直接打印或转发。",
		"ru": "Подарочные открытки во вложении одним PDF-файлом, по открытке на страницу: их можно распечатать или переслать.",
		"ar": "بطاقات هداياك مرفقة في ملف PDF واحد، بطاقة في كل صفحة، جاهزة للطباعة أو إعادة التوجيه.",
	},
	"buyer.eyebrow": {
		"en": "Gift of privacy",
		"es": "Regalo de privacidad",
		"de": "Geschenk der Privatsphäre",
		"zh": "隐私之礼",
		"ru": "Приватность в подарок",
		"ar": "هدية الخصوصية",
	},
	"buyer.headline.one": {
		"en": "Your gift is wrapped",
		"es": "Tu regalo ya está envuelto",
		"de": "Dein Geschenk ist verpackt",
		"zh": "您的礼物已包装好",
		"ru": "Ваш подарок упакован",
		"ar": "تم تغليف هديتك",
	},
	"buyer.headline.other": {
		"en": "Your gifts are wrapped",
		"es": "Tus regalos ya están envueltos",
		"de": "Deine Geschenke sind verpackt",
		"zh": "您的礼物已包装好",
		"ru": "Ваши подарки упакованы",
		"ar": "تم تغليف هداياك",
	},
	"buyer.thanks": {
		"en": "Thank you for giving the gift of privacy.",
		"es": "Gracias por regalar privacidad.",
		"de": "Danke, dass du Privatsphäre verschenkst.",
		"zh": "感谢您送出隐私这份礼物。",
		"ru": "Спасибо, что дарите приватность.",
		"ar": "شكرًا لك على منح هدية الخصوصية.",
	},
	"buyer.me.one": {
		"en": "Your gift card is attached as a PDF, ready to print or forward. Its code is one year of URnetwork Pro, starting when it's redeemed.",
		"es": "Tu tarjeta de regalo va adjunta en PDF, lista para imprimir o reenviar. Su código es un año de URnetwork Pro, que empieza cuando se canjea.",
		"de": "Deine Geschenkkarte hängt als PDF an, bereit zum Drucken oder Weiterleiten. Ihr Code ist ein Jahr URnetwork Pro, das mit dem Einlösen beginnt.",
		"zh": "您的礼品卡已作为 PDF 附上，可直接打印或转发。卡上的礼品码可兑换一年 URnetwork Pro，从兑换之日起计算。",
		"ru": "Подарочная открытка во вложении в формате PDF: её можно распечатать или переслать. Её код даёт год URnetwork Pro, который начинается с момента активации.",
		"ar": "بطاقة هديتك مرفقة بصيغة PDF، جاهزة للطباعة أو إعادة التوجيه. يمنح رمزها عامًا من URnetwork Pro يبدأ عند استرداده.",
	},
	"buyer.me.other": {
		"en": "Your gift cards are attached as one PDF, a card per page, ready to print or forward. Each code is one year of URnetwork Pro, starting when it's redeemed.",
		"es": "Tus tarjetas de regalo van adjuntas en un solo PDF, una por página, listas para imprimir o reenviar. Cada código es un año de URnetwork Pro, que empieza cuando se canjea.",
		"de": "Deine Geschenkkarten hängen als ein PDF an, eine Karte pro Seite, bereit zum Drucken oder Weiterleiten. Jeder Code ist ein Jahr URnetwork Pro, das mit dem Einlösen beginnt.",
		"zh": "您的礼品卡已合并为一个 PDF 附上（每页一张），可直接打印或转发。每个礼品码可兑换一年 URnetwork Pro，从兑换之日起计算。",
		"ru": "Подарочные открытки во вложении одним PDF-файлом, по открытке на страницу: их можно распечатать или переслать. Каждый код даёт год URnetwork Pro, который начинается с момента активации.",
		"ar": "بطاقات هداياك مرفقة في ملف PDF واحد، بطاقة في كل صفحة، جاهزة للطباعة أو إعادة التوجيه. يمنح كل رمز عامًا من URnetwork Pro يبدأ عند استرداده.",
	},
	"buyer.them_now.one": {
		"en": "We're emailing the gift card and your note now.",
		"es": "Ya estamos enviando por correo la tarjeta de regalo y tu nota.",
		"de": "Wir schicken die Geschenkkarte und deine Nachricht jetzt per E-Mail.",
		"zh": "我们正在通过邮件把礼品卡和您的留言发给好友。",
		"ru": "Мы уже отправляем по почте подарочную открытку и вашу записку.",
		"ar": "نرسل الآن بطاقة الهدية ورسالتك عبر البريد الإلكتروني.",
	},
	"buyer.them_now.other": {
		"en": "We're emailing each friend their own gift card and your note now.",
		"es": "Ya estamos enviando por correo a cada destinatario su propia tarjeta de regalo y tu nota.",
		"de": "Wir schicken jetzt jeder Person ihre eigene Geschenkkarte und deine Nachricht per E-Mail.",
		"zh": "我们正在通过邮件把各自的礼品卡和您的留言发给每位好友。",
		"ru": "Мы уже отправляем каждому получателю его подарочную открытку и вашу записку.",
		"ar": "نرسل الآن إلى كل صديق بطاقة هديته ورسالتك عبر البريد الإلكتروني.",
	},
	"buyer.them_on.one": {
		"en": "We'll email the gift card and your note on {date}.",
		"es": "Enviaremos por correo la tarjeta de regalo y tu nota el {date}.",
		"de": "Wir schicken die Geschenkkarte und deine Nachricht am {date} per E-Mail.",
		"zh": "我们将于 {date} 通过邮件把礼品卡和您的留言发给好友。",
		"ru": "Мы отправим подарочную открытку и вашу записку по почте {date}.",
		"ar": "سنرسل بطاقة الهدية ورسالتك عبر البريد الإلكتروني في {date}.",
	},
	"buyer.them_on.other": {
		"en": "We'll email each friend their own gift card and your note on {date}.",
		"es": "El {date} enviaremos por correo a cada destinatario su propia tarjeta de regalo y tu nota.",
		"de": "Am {date} schicken wir jeder Person ihre eigene Geschenkkarte und deine Nachricht per E-Mail.",
		"zh": "我们将于 {date} 通过邮件把各自的礼品卡和您的留言发给每位好友。",
		"ru": "Мы отправим каждому получателю его подарочную открытку и вашу записку по почте {date}.",
		"ar": "سنرسل إلى كل صديق بطاقة هديته ورسالتك عبر البريد الإلكتروني في {date}.",
	},
	// the card hold: a card payment's friend emails wait about two hours
	// (giftCardTime writes {time}; Spanish's carries its article)
	"buyer.them_hold.one": {
		"en": "We'll email the gift card and your note at {time}, about two hours after your payment.",
		"es": "Enviaremos por correo la tarjeta de regalo y tu nota a {time}, unas dos horas después de tu pago.",
		"de": "Wir schicken die Geschenkkarte und deine Nachricht um {time} per E-Mail, etwa zwei Stunden nach deiner Zahlung.",
		"zh": "我们将在 {time}（您付款约两小时后）通过邮件把礼品卡和您的留言发给好友。",
		"ru": "Мы отправим подарочную открытку и вашу записку по почте в {time}, примерно через два часа после оплаты.",
		"ar": "سنرسل بطاقة الهدية ورسالتك عبر البريد الإلكتروني في الساعة {time}، بعد نحو ساعتين من الدفع.",
	},
	"buyer.them_hold.other": {
		"en": "We'll email each friend their own gift card and your note at {time}, about two hours after your payment.",
		"es": "A {time}, unas dos horas después de tu pago, enviaremos por correo a cada destinatario su propia tarjeta de regalo y tu nota.",
		"de": "Um {time}, etwa zwei Stunden nach deiner Zahlung, schicken wir jeder Person ihre eigene Geschenkkarte und deine Nachricht per E-Mail.",
		"zh": "我们将在 {time}（您付款约两小时后）通过邮件把各自的礼品卡和您的留言发给每位好友。",
		"ru": "В {time}, примерно через два часа после оплаты, мы отправим каждому получателю его подарочную открытку и вашу записку.",
		"ar": "سنرسل إلى كل صديق بطاقة هديته ورسالتك عبر البريد الإلكتروني في الساعة {time}، بعد نحو ساعتين من الدفع.",
	},
	"buyer.them_copy.one": {
		"en": "The card is attached here too, so you have its code.",
		"es": "La tarjeta también va adjunta aquí, para que tengas su código.",
		"de": "Die Karte hängt auch hier an, damit du ihren Code hast.",
		"zh": "这张礼品卡也附在本邮件中，方便您保留礼品码。",
		"ru": "Открытка также приложена к этому письму, чтобы код был и у вас.",
		"ar": "البطاقة مرفقة هنا أيضًا ليبقى رمزها لديك.",
	},
	"buyer.them_copy.other": {
		"en": "Every card is attached here too, as one PDF, so you have every code.",
		"es": "Todas las tarjetas también van adjuntas aquí, en un solo PDF, para que tengas todos los códigos.",
		"de": "Alle Karten hängen auch hier an, als ein PDF, damit du jeden Code hast.",
		"zh": "所有礼品卡也已合并为一个 PDF 附在本邮件中，方便您保留每个礼品码。",
		"ru": "Все открытки также приложены к этому письму одним PDF-файлом, чтобы все коды были и у вас.",
		"ar": "جميع البطاقات مرفقة هنا أيضًا في ملف PDF واحد، لتبقى كل الرموز لديك.",
	},
	"buyer.codes.one": {
		"en": "Your code",
		"es": "Tu código",
		"de": "Dein Code",
		"zh": "礼品码",
		"ru": "Ваш код",
		"ar": "رمزك",
	},
	"buyer.codes.other": {
		"en": "Your codes",
		"es": "Tus códigos",
		"de": "Deine Codes",
		"zh": "礼品码",
		"ru": "Ваши коды",
		"ar": "رموزك",
	},
	"buyer.code_for": {
		"en": "For {email}",
		"es": "Para {email}",
		"de": "Für {email}",
		"zh": "发给 {email}",
		"ru": "Для {email}",
		"ar": "إلى {email}",
	},
	"buyer.how": {
		"en": "To redeem, scan the card's QR code or go to ur.io/redeem. The code also works in the URnetwork app: enter it when you sign in, or later under Account, Balance codes.",
		"es": "Para canjearlo, escanea el código QR de la tarjeta o entra en ur.io/redeem. El código también funciona en la app de URnetwork: introdúcelo al iniciar sesión o, más adelante, en Cuenta › Códigos de saldo.",
		"de": "Zum Einlösen den QR-Code auf der Karte scannen oder ur.io/redeem öffnen. Der Code funktioniert auch in der URnetwork-App: bei der Anmeldung eingeben oder später unter Konto › Guthabencodes.",
		"zh": "兑换时，扫描卡上的二维码或访问 ur.io/redeem。礼品码也可以在 URnetwork 应用中使用：登录时输入，或稍后在“账户 › 余额码”中输入。",
		"ru": "Чтобы активировать код, отсканируйте QR-код на открытке или откройте ur.io/redeem. Код также работает в приложении URnetwork: введите его при входе или позже в разделе «Аккаунт › Коды баланса».",
		"ar": "للاسترداد، امسح رمز QR الموجود على البطاقة أو انتقل إلى ur.io/redeem. يعمل الرمز أيضًا في تطبيق URnetwork: أدخله عند تسجيل الدخول، أو لاحقًا ضمن «الحساب › رموز الرصيد».",
	},
	"buyer.keep": {
		"en": "Anyone with a code can redeem it, so share each one only with the person it's for.",
		"es": "Cualquiera que tenga un código puede canjearlo, así que compártelo solo con la persona a la que va destinado.",
		"de": "Wer einen Code hat, kann ihn einlösen. Teile jeden Code also nur mit der Person, für die er gedacht ist.",
		"zh": "任何人拿到礼品码都可以兑换，请只把它分享给受赠人。",
		"ru": "Код может активировать любой, у кого он есть, поэтому передавайте каждый код только тому, кому он предназначен.",
		"ar": "يمكن لأي شخص لديه الرمز استرداده، لذا شارك كل رمز فقط مع الشخص المقصود به.",
	},
	"buyer.why": {
		"en": "You received this because URnetwork gift codes were bought at ur.io with this address.",
		"es": "Recibes este correo porque se compraron códigos de regalo de URnetwork en ur.io con esta dirección.",
		"de": "Du erhältst diese E-Mail, weil mit dieser Adresse URnetwork-Geschenkcodes auf ur.io gekauft wurden.",
		"zh": "您收到此邮件，是因为有人使用此邮箱地址在 ur.io 购买了 URnetwork 礼品码。",
		"ru": "Вы получили это письмо, потому что с этим адресом на ur.io были куплены подарочные коды URnetwork.",
		"ar": "تلقيت هذه الرسالة لأنه تم شراء رموز هدايا URnetwork من ur.io باستخدام هذا العنوان.",
	},

	// the friend's email (gift_card_friend)
	"friend.subject.signed": {
		"en": "{signature} sent you a gift of privacy",
		"es": "{signature} te ha enviado un regalo de privacidad",
		"de": "{signature} hat dir ein Geschenk der Privatsphäre geschickt",
		"zh": "{signature} 送您一份隐私之礼",
		"ru": "{signature} дарит вам приватность",
		"ar": "هدية الخصوصية من {signature}",
	},
	"friend.subject.anonymous": {
		"en": "Someone who cares about you sent you a gift of privacy",
		"es": "Alguien que te quiere te ha enviado un regalo de privacidad",
		"de": "Jemand, dem du am Herzen liegst, hat dir ein Geschenk der Privatsphäre geschickt",
		"zh": "一位关心您的人送您一份隐私之礼",
		"ru": "Тот, кому вы дороги, дарит вам приватность",
		"ar": "هدية الخصوصية من شخص يهتم بك",
	},
	"friend.preheader": {
		"en": "A year of URnetwork Pro, starting when you redeem it. Your gift card is attached.",
		"es": "Un año de URnetwork Pro, que empieza cuando lo canjeas. Tu tarjeta de regalo va adjunta.",
		"de": "Ein Jahr URnetwork Pro, ab dem Einlösen. Deine Geschenkkarte hängt an.",
		"zh": "一年 URnetwork Pro，从兑换之日起计算。礼品卡已随邮件附上。",
		"ru": "Год URnetwork Pro с момента активации. Подарочная открытка во вложении.",
		"ar": "عام من URnetwork Pro يبدأ عند استرداده. بطاقة هديتك مرفقة.",
	},
	"friend.eyebrow": {
		"en": "A gift for you",
		"es": "Un regalo para ti",
		"de": "Ein Geschenk für dich",
		"zh": "送您的礼物",
		"ru": "Подарок для вас",
		"ar": "هدية لك",
	},
	"friend.intro.signed": {
		"en": "{signature} gave you a year of URnetwork Pro: private, fast internet on every device. The year starts when you redeem it.",
		"es": "{signature} te ha regalado un año de URnetwork Pro: internet privado y rápido en todos tus dispositivos. El año empieza cuando lo canjeas.",
		"de": "{signature} hat dir ein Jahr URnetwork Pro geschenkt: privates, schnelles Internet auf all deinen Geräten. Das Jahr beginnt, wenn du es einlöst.",
		"zh": "{signature} 送您一年 URnetwork Pro：在所有设备上享受私密、快速的网络。这一年从您兑换之日起计算。",
		"ru": "{signature} дарит вам год URnetwork Pro: приватный и быстрый интернет на всех ваших устройствах. Год начинается с момента активации.",
		"ar": "هدية من {signature}: عام من URnetwork Pro، إنترنت خاص وسريع على جميع أجهزتك. يبدأ العام عند استرداده.",
	},
	"friend.intro.anonymous": {
		"en": "Someone who cares about you gave you a year of URnetwork Pro: private, fast internet on every device. The year starts when you redeem it.",
		"es": "Alguien que te quiere te ha regalado un año de URnetwork Pro: internet privado y rápido en todos tus dispositivos. El año empieza cuando lo canjeas.",
		"de": "Jemand, dem du am Herzen liegst, hat dir ein Jahr URnetwork Pro geschenkt: privates, schnelles Internet auf all deinen Geräten. Das Jahr beginnt, wenn du es einlöst.",
		"zh": "一位关心您的人送您一年 URnetwork Pro：在所有设备上享受私密、快速的网络。这一年从您兑换之日起计算。",
		"ru": "Тот, кому вы дороги, дарит вам год URnetwork Pro: приватный и быстрый интернет на всех ваших устройствах. Год начинается с момента активации.",
		"ar": "هدية من شخص يهتم بك: عام من URnetwork Pro، إنترنت خاص وسريع على جميع أجهزتك. يبدأ العام عند استرداده.",
	},
	"friend.attached": {
		"en": "Your gift card is attached as a PDF, to keep or print.",
		"es": "Tu tarjeta de regalo va adjunta en PDF, para guardarla o imprimirla.",
		"de": "Deine Geschenkkarte hängt als PDF an, zum Aufbewahren oder Ausdrucken.",
		"zh": "礼品卡已作为 PDF 附上，方便您保存或打印。",
		"ru": "Подарочная открытка приложена в формате PDF: её можно сохранить или распечатать.",
		"ar": "بطاقة هديتك مرفقة بصيغة PDF لتحتفظ بها أو تطبعها.",
	},
	"friend.code_label": {
		"en": "Your code",
		"es": "Tu código",
		"de": "Dein Code",
		"zh": "您的礼品码",
		"ru": "Ваш код",
		"ar": "رمزك",
	},
	"friend.button": {
		"en": "Redeem your gift",
		"es": "Canjear tu regalo",
		"de": "Geschenk einlösen",
		"zh": "兑换礼物",
		"ru": "Активировать подарок",
		"ar": "استرد هديتك",
	},
	"friend.button_hint": {
		"en": "The button opens ur.io/redeem with your code filled in.",
		"es": "El botón abre ur.io/redeem con tu código ya escrito.",
		"de": "Der Button öffnet ur.io/redeem mit deinem Code.",
		"zh": "点击按钮将打开 ur.io/redeem，并自动填入您的礼品码。",
		"ru": "Кнопка откроет ur.io/redeem с уже введённым кодом.",
		"ar": "يفتح الزر ur.io/redeem مع إدخال رمزك تلقائيًا.",
	},
	"friend.steps_title": {
		"en": "How to redeem",
		"es": "Cómo canjearlo",
		"de": "So löst du ihn ein",
		"zh": "兑换方法",
		"ru": "Как активировать",
		"ar": "طريقة الاسترداد",
	},
	"friend.reply": {
		"en": "Replies to this email go to the person who sent you this gift.",
		"es": "Las respuestas a este correo le llegan a la persona que te envió este regalo.",
		"de": "Antworten auf diese E-Mail gehen an die Person, die dir dieses Geschenk geschickt hat.",
		"zh": "回复此邮件将发送给送您这份礼物的人。",
		"ru": "Ответ на это письмо получит человек, который сделал вам этот подарок.",
		"ar": "تصل الردود على هذه الرسالة إلى الشخص الذي أرسل إليك هذه الهدية.",
	},
	"friend.why": {
		"en": "You received this because someone bought you a URnetwork gift at ur.io and entered this address. If you weren't expecting it, you can ignore this email.",
		"es": "Recibes este correo porque alguien te compró un regalo de URnetwork en ur.io e introdujo esta dirección. Si no lo esperabas, puedes ignorar este correo.",
		"de": "Du erhältst diese E-Mail, weil jemand auf ur.io ein URnetwork-Geschenk für dich gekauft und diese Adresse angegeben hat. Wenn du nicht damit gerechnet hast, kannst du sie ignorieren.",
		"zh": "您收到此邮件，是因为有人在 ur.io 为您购买了 URnetwork 礼物，并填写了此邮箱地址。如果您并未预期收到此邮件，可以忽略。",
		"ru": "Вы получили это письмо, потому что кто-то купил вам подарок URnetwork на ur.io и указал этот адрес. Если вы его не ждали, просто проигнорируйте письмо.",
		"ar": "تلقيت هذه الرسالة لأن أحدهم اشترى لك هدية من URnetwork على ur.io وأدخل هذا العنوان. إذا لم تكن تتوقعها، يمكنك تجاهل هذه الرسالة.",
	},

	// the gift-ending reminder (gift_ending, service_reminders_controller.go):
	// 14 days before a redeemed gift year ends. It offers Pro at the regular
	// price, never a discount, and names no price; product updates turn it
	// off, as the campaigns' do ("Product updates", the switch in ur.io's
	// account settings)
	"ending.title": {
		"en": "Your gift year of URnetwork Pro ends on {date}",
		"es": "Tu año de URnetwork Pro de regalo termina el {date}",
		"de": "Dein geschenktes Jahr URnetwork Pro endet am {date}",
		"zh": "您获赠的一年 URnetwork Pro 将于 {date} 结束",
		"ru": "Подаренный вам год URnetwork Pro заканчивается {date}",
		"ar": "ينتهي عام URnetwork Pro المُهدى إليك في {date}",
	},
	"ending.preheader": {
		"en": "To keep Pro after that, choose a plan at the regular price. To go back to the free plan, there's nothing to do.",
		"es": "Para seguir con Pro después, elige un plan al precio normal. Para volver al plan gratuito, no tienes que hacer nada.",
		"de": "Wenn du Pro danach behalten möchtest, wähle einen Tarif zum regulären Preis. Für den kostenlosen Tarif musst du nichts tun.",
		"zh": "如需在此之后继续使用 Pro，请按常规价格选择套餐。如果想回到免费套餐，无需任何操作。",
		"ru": "Чтобы сохранить Pro и дальше, выберите тариф по обычной цене. Чтобы вернуться на бесплатный тариф, ничего делать не нужно.",
		"ar": "للاحتفاظ بـ Pro بعد ذلك، اختر خطة بالسعر العادي. وللعودة إلى الخطة المجانية، لا داعي لفعل أي شيء.",
	},
	"ending.eyebrow": {
		"en": "Reminder",
		"es": "Recordatorio",
		"de": "Erinnerung",
		"zh": "提醒",
		"ru": "Напоминание",
		"ar": "تذكير",
	},
	"ending.body": {
		"en": "The year of URnetwork Pro from your gift code ends on {date}. To keep Pro after that, choose a plan at the regular price.",
		"es": "El año de URnetwork Pro de tu código de regalo termina el {date}. Para seguir con Pro después, elige un plan al precio normal.",
		"de": "Das Jahr URnetwork Pro aus deinem Geschenkcode endet am {date}. Wenn du Pro danach behalten möchtest, wähle einen Tarif zum regulären Preis.",
		"zh": "您通过礼品码获得的一年 URnetwork Pro 将于 {date} 结束。如需在此之后继续使用 Pro，请按常规价格选择套餐。",
		"ru": "Год URnetwork Pro по вашему подарочному коду заканчивается {date}. Чтобы сохранить Pro и дальше, выберите тариф по обычной цене.",
		"ar": "ينتهي عام URnetwork Pro الذي حصلت عليه برمز الهدية في {date}. للاحتفاظ بـ Pro بعد ذلك، اختر خطة بالسعر العادي.",
	},
	"ending.button": {
		"en": "Keep URnetwork Pro",
		"es": "Seguir con URnetwork Pro",
		"de": "URnetwork Pro behalten",
		"zh": "继续使用 URnetwork Pro",
		"ru": "Сохранить URnetwork Pro",
		"ar": "الاحتفاظ بـ URnetwork Pro",
	},
	"ending.free": {
		"en": "If you'd rather go back to the free plan, there's nothing to do. Questions? Contact support@ur.io.",
		"es": "Si prefieres volver al plan gratuito, no tienes que hacer nada. ¿Preguntas? Escribe a support@ur.io.",
		"de": "Wenn du lieber zum kostenlosen Tarif zurückkehren möchtest, musst du nichts tun. Fragen? Schreib an support@ur.io.",
		"zh": "如果您想回到免费套餐，无需任何操作。有疑问？请联系 support@ur.io。",
		"ru": "Если вы предпочитаете вернуться на бесплатный тариф, ничего делать не нужно. Есть вопросы? Напишите на support@ur.io.",
		"ar": "إذا كنت تفضل العودة إلى الخطة المجانية، فلا داعي لفعل أي شيء. لديك أسئلة؟ راسلنا على support@ur.io.",
	},
	"ending.why": {
		"en": "Sent once to the address on your URnetwork account, 14 days before a year of URnetwork Pro from a gift code ends, because product updates are on. To stop emails like this, turn product updates off at ur.io/app/account/settings.",
		"es": "Se envía una sola vez a la dirección de tu cuenta de URnetwork, 14 días antes de que termine un año de URnetwork Pro de un código de regalo, porque tienes activadas las novedades del producto. Para dejar de recibir correos como este, desactívalas en ur.io/app/account/settings.",
		"de": "Einmalig an die Adresse deines URnetwork-Kontos gesendet, 14 Tage bevor ein Jahr URnetwork Pro aus einem Geschenkcode endet, weil Produkt-Updates aktiviert sind. Wenn du solche E-Mails nicht mehr erhalten möchtest, deaktiviere Produkt-Updates unter ur.io/app/account/settings.",
		"zh": "由于您开启了产品更新，我们会在通过礼品码获得的一年 URnetwork Pro 结束前 14 天，向您 URnetwork 账户的邮箱发送一次此邮件。如不想再收到此类邮件，请在 ur.io/app/account/settings 关闭产品更新。",
		"ru": "Это письмо отправляется один раз на адрес вашего аккаунта URnetwork за 14 дней до окончания года URnetwork Pro по подарочному коду, потому что у вас включены новости о продукте. Чтобы больше не получать такие письма, отключите их на ur.io/app/account/settings.",
		"ar": "تُرسل هذه الرسالة مرة واحدة إلى عنوان حسابك على URnetwork قبل 14 يومًا من انتهاء عام URnetwork Pro من رمز هدية، لأن تحديثات المنتج مفعّلة لديك. لإيقاف رسائل كهذه، أوقف تحديثات المنتج من ur.io/app/account/settings.",
	},
}

// The string for the key in the language, with each `{name}` replaced by the
// value that follows that name in `args`. An unknown key panics: every key the
// templates use is in giftCardStrings (TestGiftEmailTemplatesRenderEveryLanguage).
func giftCardText(language string, key string, args ...string) string {
	translations, ok := giftCardStrings[key]
	if !ok {
		panic(fmt.Errorf("gift card string %q does not exist", key))
	}
	text, ok := translations[giftCardLanguage(language)]
	if !ok {
		text = translations[giftCardLanguages[0]]
	}
	if 0 < len(args) {
		pairs := []string{}
		for i := 0; i+1 < len(args); i += 2 {
			pairs = append(pairs, "{"+args[i]+"}", args[i+1])
		}
		text = strings.NewReplacer(pairs...).Replace(text)
	}
	return text
}

// The form of the key for `count` gifts: key.one for one, key.other for more.
func giftCardTextCount(language string, key string, count int, args ...string) string {
	if count == 1 {
		return giftCardText(language, key+".one", args...)
	}
	return giftCardText(language, key+".other", args...)
}

var (
	giftCardMonthsEn = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	giftCardMonthsEs = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}
	giftCardMonthsDe = []string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}
	// the genitive, as a date reads ("13 ноября")
	giftCardMonthsRu = []string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
	giftCardMonthsAr = []string{"يناير", "فبراير", "مارس", "أبريل", "مايو", "يونيو", "يوليو", "أغسطس", "سبتمبر", "أكتوبر", "نوفمبر", "ديسمبر"}
)

// The calendar date of `t` (in t's own location) as the language writes a
// date, the way ur.io's catalogs do: "November 13, 2027", "13 de noviembre de
// 2027", "13. November 2027", "2027 年 11 月 13 日", "13 ноября 2027 г.",
// "13 نوفمبر 2027" (Arabic with Western digits, as the site writes them).
func giftCardDate(language string, t time.Time) string {
	year, month, day := t.Date()
	index := int(month) - 1
	switch giftCardLanguage(language) {
	case "es":
		return fmt.Sprintf("%d de %s de %d", day, giftCardMonthsEs[index], year)
	case "de":
		return fmt.Sprintf("%d. %s %d", day, giftCardMonthsDe[index], year)
	case "zh":
		return fmt.Sprintf("%d 年 %d 月 %d 日", year, int(month), day)
	case "ru":
		return fmt.Sprintf("%d %s %d г.", day, giftCardMonthsRu[index], year)
	case "ar":
		return fmt.Sprintf("%d %s %d", day, giftCardMonthsAr[index], year)
	default:
		return fmt.Sprintf("%s %d, %d", giftCardMonthsEn[index], day, year)
	}
}

// The time of day of `t` (in t's own location) as the language writes it,
// with "UTC" after it when t is in UTC (a buyer whose zone is unknown): "3:04
// PM", "las 15:04" (Spanish carries the article its hour takes: "la 1:04"),
// "15:04 Uhr", and "15:04" in Chinese, Russian and Arabic (Western digits, as
// the site writes them).
func giftCardTime(language string, t time.Time) string {
	hour, minute := t.Hour(), t.Minute()
	utc := t.Location() == time.UTC
	var text string
	switch giftCardLanguage(language) {
	case "en":
		hour12 := hour % 12
		if hour12 == 0 {
			hour12 = 12
		}
		meridiem := "AM"
		if 12 <= hour {
			meridiem = "PM"
		}
		text = fmt.Sprintf("%d:%02d %s", hour12, minute, meridiem)
	case "es":
		article := "las"
		if hour == 1 {
			article = "la"
		}
		text = fmt.Sprintf("%s %d:%02d", article, hour, minute)
	case "de":
		text = fmt.Sprintf("%d:%02d", hour, minute)
		if !utc {
			text += " Uhr"
		}
	default:
		text = fmt.Sprintf("%d:%02d", hour, minute)
	}
	if utc {
		text += " UTC"
	}
	return text
}
