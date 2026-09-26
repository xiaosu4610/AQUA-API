// 本文件是 i18n 的消息目录数据（纯 Go map，无任何外部依赖）。
//
// 意图（Why）：
//
//	把"用户可见的错误文案"集中在一处，按语义化键组织，便于通读、
//	核对翻译完整性与批量维护。翻译目录刻意用纯 Go map 而非外部文件，
//	是为了让单二进制部署时无需额外分发资源、也不引入第三方依赖。
//
// 流转（Flow）：
//
//	Bundle.Message(locale, key) → catalog[key][locale]（缺失则 catalog[key][Default]）
//
// 扩展（Extend）：
//
//	新增词条时，务必为「六种语言的同一键」同时补齐翻译；
//	缺任何一门都会让界面出现"半翻译"，i18n_test.go 会强制校验这一点。
//	键名用语义化英文（领域.动作），不要用中文或具体文案当键。
package i18n

// catalog 是内置消息目录。
//
// 组织方式：语义化键 → 语言 → 文案。中文（Default）为回退基准，
// 因此每条都必须有中文；其余语言缺失时由 Message 自动回退到中文。
var catalog = Catalog{
	// ── 请求体相关（面向调用方，会被直接展示）────────────────────────
	"request.invalid_json": {
		ZhCN: "请求体格式错误",
		En:   "Malformed request body",
		Fr:   "Corps de requête mal formé",
		Ru:   "Некорректное тело запроса",
		Es:   "Cuerpo de la solicitud con formato incorrecto",
		Ar:   "جسم الطلب غير صالح",
	},
	"request.malformed_json": {
		ZhCN: "请求体不是合法的 JSON",
		En:   "The request body is not valid JSON",
		Fr:   "Le corps de la requête n'est pas un JSON valide",
		Ru:   "Тело запроса не является допустимым JSON",
		Es:   "El cuerpo de la solicitud no es un JSON válido",
		Ar:   "جسم الطلب ليس بتنسيق JSON صالح",
	},
	"request.too_large": {
		ZhCN: "请求体超过上限",
		En:   "The request body exceeds the size limit",
		Fr:   "Le corps de la requête dépasse la taille maximale",
		Ru:   "Размер тела запроса превышает допустимый предел",
		Es:   "El cuerpo de la solicitud supera el tamaño máximo",
		Ar:   "حجم جسم الطلب يتجاوز الحد المسموح",
	},
	"request.missing_model": {
		ZhCN: "缺少 model 字段",
		En:   `The request is missing the "model" field`,
		Fr:   "Le champ « model » est manquant",
		Ru:   "Отсутствует поле «model»",
		Es:   `Falta el campo "model"`,
		Ar:   `الحقل "model" مفقود`,
	},

	// ── 访问令牌与账号（/v1 模型接口鉴权，面向调用方）────────────────
	"auth.missing_token": {
		ZhCN: "缺少访问令牌，请在 Authorization 头中携带 Bearer <令牌>",
		En:   "Missing access token. Provide a Bearer token in the Authorization header.",
		Fr:   "Jeton d'accès manquant. Fournissez un jeton Bearer dans l'en-tête Authorization.",
		Ru:   "Отсутствует токен доступа. Укажите токен Bearer в заголовке Authorization.",
		Es:   "Falta el token de acceso. Incluya un token Bearer en el encabezado Authorization.",
		Ar:   "رمز الوصول مفقود. يُرجى إرسال رمز Bearer في ترويسة Authorization.",
	},
	"auth.invalid_token": {
		ZhCN: "访问令牌无效",
		En:   "Invalid access token",
		Fr:   "Jeton d'accès non valide",
		Ru:   "Недействительный токен доступа",
		Es:   "Token de acceso no válido",
		Ar:   "رمز وصول غير صالح",
	},
	"auth.token_disabled": {
		ZhCN: "访问令牌已被禁用",
		En:   "The access token has been disabled",
		Fr:   "Le jeton d'accès a été désactivé",
		Ru:   "Токен доступа отключён",
		Es:   "El token de acceso ha sido deshabilitado",
		Ar:   "تم تعطيل رمز الوصول",
	},
	"auth.token_expired": {
		ZhCN: "访问令牌已过期",
		En:   "The access token has expired",
		Fr:   "Le jeton d'accès a expiré",
		Ru:   "Срок действия токена доступа истёк",
		Es:   "El token de acceso ha caducado",
		Ar:   "انتهت صلاحية رمز الوصول",
	},
	"auth.token_revoked": {
		ZhCN: "访问令牌已失效",
		En:   "The access token is no longer valid",
		Fr:   "Le jeton d'accès n'est plus valide",
		Ru:   "Токен доступа больше не действителен",
		Es:   "El token de acceso ya no es válido",
		Ar:   "لم يعد رمز الوصول صالحًا",
	},
	"auth.account_disabled": {
		ZhCN: "账号已被禁用",
		En:   "The account has been disabled",
		Fr:   "Le compte a été désactivé",
		Ru:   "Учётная запись отключена",
		Es:   "La cuenta ha sido deshabilitada",
		Ar:   "تم تعطيل الحساب",
	},
	"auth.account_missing": {
		ZhCN: "账号不存在",
		En:   "Account not found",
		Fr:   "Compte introuvable",
		Ru:   "Учётная запись не найдена",
		Es:   "Cuenta no encontrada",
		Ar:   "الحساب غير موجود",
	},
	"auth.not_logged_in": {
		ZhCN: "未登录",
		En:   "Not signed in",
		Fr:   "Non connecté",
		Ru:   "Вы не вошли в систему",
		Es:   "No ha iniciado sesión",
		Ar:   "لم يتم تسجيل الدخول",
	},
	"auth.credentials_missing": {
		ZhCN: "未登录或凭据缺失",
		En:   "Not signed in or credentials missing",
		Fr:   "Non connecté ou identifiants manquants",
		Ru:   "Вы не вошли в систему или отсутствуют учётные данные",
		Es:   "No ha iniciado sesión o faltan credenciales",
		Ar:   "لم يتم تسجيل الدخول أو أن بيانات الاعتماد مفقودة",
	},
	"auth.session_invalid": {
		ZhCN: "登录已失效，请重新登录",
		En:   "Your session is no longer valid. Please sign in again.",
		Fr:   "Votre session n'est plus valide. Veuillez vous reconnecter.",
		Ru:   "Сессия недействительна. Войдите снова.",
		Es:   "Su sesión ya no es válida. Vuelva a iniciar sesión.",
		Ar:   "انتهت صلاحية جلستك. يُرجى تسجيل الدخول مرة أخرى.",
	},
	"auth.session_expired": {
		ZhCN: "登录已过期，请重新登录",
		En:   "Your session has expired. Please sign in again.",
		Fr:   "Votre session a expiré. Veuillez vous reconnecter.",
		Ru:   "Срок действия сессии истёк. Войдите снова.",
		Es:   "Su sesión ha caducado. Vuelva a iniciar sesión.",
		Ar:   "انتهت صلاحية جلستك. يُرجى تسجيل الدخول مرة أخرى.",
	},
	"auth.admin_required": {
		ZhCN: "需要管理员权限",
		En:   "Administrator privileges required",
		Fr:   "Droits d'administrateur requis",
		Ru:   "Требуются права администратора",
		Es:   "Se requieren privilegios de administrador",
		Ar:   "مطلوب صلاحيات المسؤول",
	},
	"auth.invalid_credentials": {
		ZhCN: "用户名或密码错误",
		En:   "Incorrect username or password",
		Fr:   "Nom d'utilisateur ou mot de passe incorrect",
		Ru:   "Неверное имя пользователя или пароль",
		Es:   "Nombre de usuario o contraseña incorrectos",
		Ar:   "اسم المستخدم أو كلمة المرور غير صحيحة",
	},
	"auth.username_taken": {
		ZhCN: "用户名已被占用",
		En:   "The username is already taken",
		Fr:   "Ce nom d'utilisateur est déjà utilisé",
		Ru:   "Имя пользователя уже занято",
		Es:   "El nombre de usuario ya está en uso",
		Ar:   "اسم المستخدم مستخدم بالفعل",
	},
	"auth.invalid_email": {
		ZhCN: "邮箱格式不正确",
		En:   "Invalid email format",
		Fr:   "Format d'adresse e-mail invalide",
		Ru:   "Неверный формат адреса электронной почты",
		Es:   "Formato de correo electrónico no válido",
		Ar:   "تنسيق البريد الإلكتروني غير صالح",
	},
	"auth.invalid_registration": {
		ZhCN: "注册信息不符合要求",
		En:   "The registration details are not acceptable",
		Fr:   "Les informations d'inscription ne sont pas valides",
		Ru:   "Данные регистрации не соответствуют требованиям",
		Es:   "Los datos de registro no son válidos",
		Ar:   "بيانات التسجيل غير مقبولة",
	},
	"auth.register_disabled": {
		ZhCN: "本站当前未开放注册，请联系管理员开通账号",
		En:   "Registration is currently closed. Please contact the administrator to get an account.",
		Fr:   "Les inscriptions sont actuellement fermées. Veuillez contacter l'administrateur pour obtenir un compte.",
		Ru:   "Регистрация временно закрыта. Обратитесь к администратору для получения учётной записи.",
		Es:   "El registro está cerrado actualmente. Contacte con el administrador para obtener una cuenta.",
		Ar:   "التسجيل مغلق حاليًا. يُرجى التواصل مع المسؤول للحصول على حساب.",
	},
	"auth.registration_closed": {
		ZhCN: "本站当前未开放注册",
		En:   "Registration is currently closed on this site",
		Fr:   "Les inscriptions sont actuellement fermées sur ce site",
		Ru:   "Регистрация на этом сайте временно закрыта",
		Es:   "El registro está cerrado actualmente en este sitio",
		Ar:   "التسجيل مغلق حاليًا في هذا الموقع",
	},

	// ── 额度相关（面向调用方，含精确数值便于自助定位）───────────────
	"quota.token_exhausted": {
		ZhCN: "访问令牌额度已用尽",
		En:   "The access token has run out of quota",
		Fr:   "Le quota du jeton d'accès est épuisé",
		Ru:   "Квота токена доступа исчерпана",
		Es:   "El token de acceso ha agotado su cuota",
		Ar:   "نفدت حصة رمز الوصول",
	},
	"quota.account_exhausted": {
		ZhCN: "账号额度已用尽（额度 %d，已用 %d，在途预留 %d），请联系管理员调整额度",
		En:   "The account quota is exhausted (quota %d, used %d, reserved %d). Please contact the administrator to adjust it.",
		Fr:   "Le quota du compte est épuisé (quota %d, utilisé %d, réservé %d). Veuillez contacter l'administrateur pour l'ajuster.",
		Ru:   "Квота учётной записи исчерпана (квота %d, использовано %d, зарезервировано %d). Обратитесь к администратору.",
		Es:   "La cuota de la cuenta está agotada (cuota %d, usada %d, reservada %d). Contacte con el administrador para ajustarla.",
		Ar:   "نفدت حصة الحساب (الحصة %d، المستخدم %d، المحجوز %d). يُرجى التواصل مع المسؤول لتعديل الحصة.",
	},
	"quota.insufficient": {
		ZhCN: "账号额度不足（剩余 %d，本次预计需要 %d），请联系管理员充值或调整额度",
		En:   "Insufficient account quota (remaining %d, this request needs about %d). Please contact the administrator to top up or adjust the quota.",
		Fr:   "Quota du compte insuffisant (restant %d, cette requête nécessite environ %d). Veuillez contacter l'administrateur.",
		Ru:   "Недостаточно квоты учётной записи (осталось %d, требуется около %d). Обратитесь к администратору.",
		Es:   "Cuota de la cuenta insuficiente (restante %d, esta solicitud necesita unos %d). Contacte con el administrador.",
		Ar:   "حصة الحساب غير كافية (المتبقي %d، المطلوب لهذا الطلب حوالي %d). يُرجى التواصل مع المسؤول.",
	},

	// ── 模型访问控制（面向调用方）────────────────────────────────────
	"model.not_allowed": {
		ZhCN: "该令牌无权访问指定模型",
		En:   "This token is not allowed to access the requested model",
		Fr:   "Ce jeton n'est pas autorisé à accéder au modèle demandé",
		Ru:   "Этот токен не имеет доступа к запрошенной модели",
		Es:   "Este token no tiene permiso para acceder al modelo solicitado",
		Ar:   "هذا الرمز غير مصرّح له بالوصول إلى النموذج المطلوب",
	},

	// ── 注册邮箱验证码（公开注册流程，面向最终用户）──────────────────
	"email.code_not_required": {
		ZhCN: "本站注册无需邮箱验证码",
		En:   "Email verification is not required for registration on this site",
		Fr:   "La vérification par e-mail n'est pas requise pour l'inscription sur ce site",
		Ru:   "Подтверждение по электронной почте при регистрации не требуется",
		Es:   "No se requiere verificación por correo electrónico para registrarse en este sitio",
		Ar:   "لا يلزم التحقق بالبريد الإلكتروني للتسجيل في هذا الموقع",
	},
	"email.service_unavailable": {
		ZhCN: "邮件服务尚未配置，请联系站点管理员",
		En:   "The email service is not configured yet. Please contact the site administrator.",
		Fr:   "Le service d'e-mail n'est pas encore configuré. Veuillez contacter l'administrateur du site.",
		Ru:   "Служба электронной почты ещё не настроена. Обратитесь к администратору сайта.",
		Es:   "El servicio de correo electrónico aún no está configurado. Contacte con el administrador del sitio.",
		Ar:   "لم يتم إعداد خدمة البريد الإلكتروني بعد. يُرجى التواصل مع مسؤول الموقع.",
	},
	"email.cooldown": {
		ZhCN: "请求过于频繁，请 %d 秒后重试",
		En:   "Too many requests. Please retry in %d seconds.",
		Fr:   "Trop de requêtes. Veuillez réessayer dans %d secondes.",
		Ru:   "Слишком много запросов. Повторите попытку через %d секунд.",
		Es:   "Demasiadas solicitudes. Vuelva a intentarlo en %d segundos.",
		Ar:   "طلبات كثيرة جدًا. يُرجى إعادة المحاولة بعد %d ثانية.",
	},
	"email.rate_email": {
		ZhCN: "该邮箱申请验证码过于频繁，请稍后再试",
		En:   "Too many verification codes requested for this email. Please try again later.",
		Fr:   "Trop de codes de vérification demandés pour cet e-mail. Veuillez réessayer plus tard.",
		Ru:   "Слишком частые запросы кодов подтверждения для этого адреса. Повторите позже.",
		Es:   "Se han solicitado demasiados códigos de verificación para este correo. Inténtelo más tarde.",
		Ar:   "تم طلب رموز تحقق كثيرة جدًا لهذا البريد. يُرجى المحاولة لاحقًا.",
	},
	"email.rate_ip": {
		ZhCN: "当前网络申请验证码过于频繁，请稍后再试",
		En:   "Too many verification codes requested from this network. Please try again later.",
		Fr:   "Trop de codes de vérification demandés depuis ce réseau. Veuillez réessayer plus tard.",
		Ru:   "Слишком частые запросы кодов подтверждения из этой сети. Повторите позже.",
		Es:   "Se han solicitado demasiados códigos de verificación desde esta red. Inténtelo más tarde.",
		Ar:   "تم طلب رموز تحقق كثيرة جدًا من هذه الشبكة. يُرجى المحاولة لاحقًا.",
	},
	"email.send_failed": {
		ZhCN: "验证码邮件发送失败，请稍后重试",
		En:   "Failed to send the verification email. Please try again later.",
		Fr:   "L'envoi de l'e-mail de vérification a échoué. Veuillez réessayer plus tard.",
		Ru:   "Не удалось отправить письмо с кодом подтверждения. Повторите позже.",
		Es:   "No se pudo enviar el correo de verificación. Inténtelo más tarde.",
		Ar:   "فشل إرسال رسالة التحقق. يُرجى المحاولة لاحقًا.",
	},
	"email.required": {
		ZhCN: "请填写邮箱",
		En:   "Please enter your email address",
		Fr:   "Veuillez saisir votre adresse e-mail",
		Ru:   "Укажите адрес электронной почты",
		Es:   "Introduzca su correo electrónico",
		Ar:   "يُرجى إدخال بريدك الإلكتروني",
	},
	"email.code_required": {
		ZhCN: "请填写邮箱验证码",
		En:   "Please enter the email verification code",
		Fr:   "Veuillez saisir le code de vérification",
		Ru:   "Укажите код подтверждения из письма",
		Es:   "Introduzca el código de verificación",
		Ar:   "يُرجى إدخال رمز التحقق",
	},
	"email.code_invalid": {
		ZhCN: "验证码不存在或已被使用，请重新获取",
		En:   "The verification code does not exist or has already been used. Please request a new one.",
		Fr:   "Le code de vérification n'existe pas ou a déjà été utilisé. Veuillez en demander un nouveau.",
		Ru:   "Код подтверждения не существует или уже использован. Запросите новый.",
		Es:   "El código de verificación no existe o ya se ha utilizado. Solicite uno nuevo.",
		Ar:   "رمز التحقق غير موجود أو تم استخدامه بالفعل. يُرجى طلب رمز جديد.",
	},
	"email.code_expired": {
		ZhCN: "验证码已过期，请重新获取",
		En:   "The verification code has expired. Please request a new one.",
		Fr:   "Le code de vérification a expiré. Veuillez en demander un nouveau.",
		Ru:   "Срок действия кода подтверждения истёк. Запросите новый.",
		Es:   "El código de verificación ha caducado. Solicite uno nuevo.",
		Ar:   "انتهت صلاحية رمز التحقق. يُرجى طلب رمز جديد.",
	},
	"email.code_attempts_exceeded": {
		ZhCN: "验证码尝试次数过多，请重新获取",
		En:   "Too many verification attempts. Please request a new code.",
		Fr:   "Trop de tentatives de vérification. Veuillez demander un nouveau code.",
		Ru:   "Слишком много попыток. Запросите новый код.",
		Es:   "Demasiados intentos de verificación. Solicite un código nuevo.",
		Ar:   "محاولات تحقق كثيرة جدًا. يُرجى طلب رمز جديد.",
	},
	"email.code_mismatch": {
		ZhCN: "验证码错误，还可尝试 %d 次",
		En:   "Incorrect verification code. You have %d attempts left.",
		Fr:   "Code de vérification incorrect. Il vous reste %d tentatives.",
		Ru:   "Неверный код подтверждения. Осталось попыток: %d.",
		Es:   "Código de verificación incorrecto. Le quedan %d intentos.",
		Ar:   "رمز التحقق غير صحيح. تبقّى لك %d محاولات.",
	},
	"email.code_used": {
		ZhCN: "验证码已被使用，请重新获取",
		En:   "The verification code has already been used. Please request a new one.",
		Fr:   "Le code de vérification a déjà été utilisé. Veuillez en demander un nouveau.",
		Ru:   "Код подтверждения уже использован. Запросите новый.",
		Es:   "El código de verificación ya se ha utilizado. Solicite uno nuevo.",
		Ar:   "تم استخدام رمز التحقق بالفعل. يُرجى طلب رمز جديد.",
	},

	// ── 访问令牌与分组（门户面向用户，管理员后台同样受用）────────────
	"token.name_required": {
		ZhCN: "令牌名称不能为空",
		En:   "The token name cannot be empty",
		Fr:   "Le nom du jeton ne peut pas être vide",
		Ru:   "Имя токена не может быть пустым",
		Es:   "El nombre del token no puede estar vacío",
		Ar:   "لا يمكن أن يكون اسم الرمز فارغًا",
	},
	"token.not_found": {
		ZhCN: "令牌不存在",
		En:   "Token not found",
		Fr:   "Jeton introuvable",
		Ru:   "Токен не найден",
		Es:   "Token no encontrado",
		Ar:   "الرمز غير موجود",
	},
	"token.invalid_status": {
		ZhCN: "令牌状态非法",
		En:   "Invalid token status",
		Fr:   "Statut de jeton non valide",
		Ru:   "Недопустимый статус токена",
		Es:   "Estado del token no válido",
		Ar:   "حالة الرمز غير صالحة",
	},
	"group.not_found": {
		ZhCN: "分组不存在",
		En:   "Group not found",
		Fr:   "Groupe introuvable",
		Ru:   "Группа не найдена",
		Es:   "Grupo no encontrado",
		Ar:   "المجموعة غير موجودة",
	},

	// ── 兑换码（用户在门户兑换额度）─────────────────────────────────
	"redeem.not_found": {
		ZhCN: "兑换码不存在，请检查是否输入有误",
		En:   "The redemption code does not exist. Please check your input.",
		Fr:   "Le code de recharge n'existe pas. Veuillez vérifier votre saisie.",
		Ru:   "Код активации не найден. Проверьте ввод.",
		Es:   "El código de canje no existe. Compruebe lo que ha introducido.",
		Ar:   "رمز الاستبدال غير موجود. يُرجى التحقق من الإدخال.",
	},
	"redeem.used": {
		ZhCN: "该兑换码已被使用",
		En:   "This redemption code has already been used",
		Fr:   "Ce code de recharge a déjà été utilisé",
		Ru:   "Этот код активации уже использован",
		Es:   "Este código de canje ya se ha utilizado",
		Ar:   "تم استخدام رمز الاستبدال هذا بالفعل",
	},
	"redeem.expired": {
		ZhCN: "该兑换码已过期",
		En:   "This redemption code has expired",
		Fr:   "Ce code de recharge a expiré",
		Ru:   "Срок действия этого кода активации истёк",
		Es:   "Este código de canje ha caducado",
		Ar:   "انتهت صلاحية رمز الاستبدال هذا",
	},
	"redeem.void": {
		ZhCN: "该兑换码已作废",
		En:   "This redemption code has been voided",
		Fr:   "Ce code de recharge a été annulé",
		Ru:   "Этот код активации аннулирован",
		Es:   "Este código de canje ha sido anulado",
		Ar:   "تم إلغاء رمز الاستبدال هذا",
	},
	"redeem.code_required": {
		ZhCN: "请填写兑换码",
		En:   "Please enter the redemption code",
		Fr:   "Veuillez saisir le code de recharge",
		Ru:   "Введите код активации",
		Es:   "Introduzca el código de canje",
		Ar:   "يُرجى إدخال رمز الاستبدال",
	},

	// ── 异步任务（用户在门户查询自己的任务）─────────────────────────
	"task.not_found": {
		ZhCN: "任务不存在",
		En:   "Task not found",
		Fr:   "Tâche introuvable",
		Ru:   "Задача не найдена",
		Es:   "Tarea no encontrada",
		Ar:   "المهمة غير موجودة",
	},
}
