<?php
/**
 * 注册邮箱验证码的端到端测试（真服务、真会话、真提交）。
 *
 * 要守住的三件事：
 *   1. **没填验证码就注册不进来** —— 这是站长开这个开关的全部意义；
 *   2. **验证码只能用一次、且会过期** —— 否则它等同于永久通行证；
 *   3. **发不出邮件时如实告知** —— 假装发成功，用户会在「为什么没收到」上耗很久。
 *
 * 测试不依赖外网：SMTP 指向 127.0.0.1 上一个没人监听的端口（连接立刻被拒），
 * 邮箱真实性探测也关掉。验证码本身由测试直接调用 EmailCode::issue() 取得，
 * 这样「校验逻辑」与「发信通道」可以分别验证，互不干扰。
 *
 * 用法：php scripts/test-register-email-code.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\EmailCode;
use app\common\Schema;
use app\common\Settings;
use app\common\User;

$dbFile = probe_test_temp('regcode-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();

// ── 打开「注册 + 邮箱验证」这条路 ──
Settings::put('register.open', true);
Settings::put('register.need_verify', true);
Settings::put('register.invite_code', '');
Settings::put('register.gift_balance', 0);
Settings::put('register.default_token_quota', 0);
// 关掉 MX / SMTP 收件人探测：离线环境连不出去，会让注册直接失败
Settings::put('register.verify_email', false);

// 邮件「配置齐全但服务器不存在」：用于验证「发信失败会被如实告知」
Settings::put('mail.enabled', true);
Settings::put('mail.host', '127.0.0.1');
Settings::put('mail.port', 1);
Settings::put('mail.secure', 'none');
Settings::put('mail.username', 'tester');
Settings::put('mail.password', 'tester');
Settings::put('mail.from', 'tester@example.com');

$password = 'Passw0rd!2026';
$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('regcode-cookies', '.txt');
$csrf = '';

echo "一、注册页长什么样\n";

$page = probe_http($port, 'GET', '/register', [], $jar);
check('注册页可访问', $page['status'] === 200, (string) $page['status']);
check('出现了验证码输入框', str_contains($page['body'], 'name="email_code"'));
check('出现了「获取验证码」按钮', str_contains($page['body'], 'id="code-btn"'));
check('说明了验证码怎么用', str_contains($page['body'], '获取验证码'));
check('邮箱输入框与其它输入框同一宽度（不再退回默认宽度）', str_contains($page['body'], 'type="email"') && str_contains($page['body'], 'input:not([type])'));

$csrf = probe_csrf($port, '/register', $jar);
check('取到 CSRF 令牌', $csrf !== '');

echo "\n二、没有验证码就注册不进来\n";

$noCode = probe_http($port, 'POST', '/register', [
    '_csrf' => $csrf, 'email' => 'a1@example.com', 'password' => $password, 'password2' => $password,
], $jar);
check('不填验证码 → 被拦下并提示先获取', str_contains($noCode['body'], '请先获取邮箱验证码'), '页面没给出该提示');
check('未创建账号', User::findByEmail('a1@example.com') === null);

// 先给这个邮箱真发一条码，再提交一个**不同**的数字 ——
// 这样测的才是「码不对」这条分支；若不发码就提交，命中的其实是「没有可用验证码」
EmailCode::issue('a1@example.com', EmailCode::PURPOSE_REGISTER, '127.0.0.1');
$wrong = probe_http($port, 'POST', '/register', [
    '_csrf' => $csrf, 'email' => 'a1@example.com', 'password' => $password, 'password2' => $password,
    'email_code' => '000001',
], $jar);
check('验证码不对 → 被拦下并提示不正确', str_contains($wrong['body'], '验证码不正确'), '页面没给出该提示');
check('错误验证码同样没有创建账号', User::findByEmail('a1@example.com') === null);

echo "\n三、发码端点\n";

$noCsrf = probe_http($port, 'POST', '/register/code', ['email' => 'a1@example.com'], $jar);
$noCsrfBody = json_decode($noCsrf['body'], true);
check('没有 CSRF → 被拒', ($noCsrfBody['ok'] ?? true) === false);
check('提示重新提交', str_contains((string) ($noCsrfBody['message'] ?? ''), '过期'));

$badEmail = json_decode(probe_http($port, 'POST', '/register/code', ['_csrf' => $csrf, 'email' => '不是邮箱'], $jar)['body'], true);
check('邮箱格式不对 → 被拒', ($badEmail['ok'] ?? true) === false);
check('提示先填正确邮箱', str_contains((string) ($badEmail['message'] ?? ''), '邮箱'));

$send = json_decode(probe_http($port, 'POST', '/register/code', ['_csrf' => $csrf, 'email' => 'a2@example.com'], $jar)['body'], true);
check('邮件发不出去时如实告知（不假装成功）', ($send['ok'] ?? true) === false, json_encode($send, JSON_UNESCAPED_UNICODE));
check('提示写的是发送失败', str_contains((string) ($send['message'] ?? ''), '失败'), (string) ($send['message'] ?? ''));

echo "\n四、验证码本身的三条限制\n";

$issued = EmailCode::issue('a3@example.com', EmailCode::PURPOSE_REGISTER, '127.0.0.1');
check('能发出 6 位验证码', $issued['ok'] === true && strlen($issued['code']) === 6, (string) $issued['code']);

$again = EmailCode::issue('a3@example.com', EmailCode::PURPOSE_REGISTER, '127.0.0.1');
check('60 秒内不能重复发（防骚扰）', $again['ok'] === false && $again['retryAfter'] > 0, json_encode($again, JSON_UNESCAPED_UNICODE));

$decoy = $issued['code'] === '123456' ? '654321' : '123456';
$bad = EmailCode::verify('a3@example.com', EmailCode::PURPOSE_REGISTER, $decoy);
check('错误验证码被拒', $bad['ok'] === false);
check('提示里说明还能试几次', str_contains($bad['message'], '还可以试'), $bad['message']);

$okCode = EmailCode::verify('a3@example.com', EmailCode::PURPOSE_REGISTER, $issued['code']);
check('正确验证码通过', $okCode['ok'] === true, $okCode['message']);

$reuse = EmailCode::verify('a3@example.com', EmailCode::PURPOSE_REGISTER, $issued['code']);
check('同一验证码不能重复使用', $reuse['ok'] === false, $reuse['message']);

// 过期：直接把有效期改到过去
$expired = EmailCode::issue('a4@example.com', EmailCode::PURPOSE_REGISTER, '127.0.0.1');
app\common\Db::execute(
    "UPDATE email_codes SET expires_at = ? WHERE email = ? AND purpose = 'register'",
    [time() - 10, 'a4@example.com']
);
$expiredCheck = EmailCode::verify('a4@example.com', EmailCode::PURPOSE_REGISTER, $expired['code']);
check('过期验证码被拒', $expiredCheck['ok'] === false && str_contains($expiredCheck['message'], '过期'), $expiredCheck['message']);

echo "\n五、填对验证码 → 注册成功且邮箱即为已验证\n";

$code = EmailCode::issue('a5@example.com', EmailCode::PURPOSE_REGISTER, '127.0.0.1')['code'];
$registered = probe_http($port, 'POST', '/register', [
    '_csrf' => $csrf, 'email' => 'a5@example.com', 'password' => $password, 'password2' => $password,
    'email_code' => $code,
], $jar);
check('注册成功', str_contains($registered['body'], '注册成功'), mb_substr($registered['body'], 0, 200));
$user = User::findByEmail('a5@example.com');
check('账号已创建', $user !== null);
check('邮箱标记为已验证（不必再点邮件链接）', $user !== null && (int) ($user['email_verified_at'] ?? 0) > 0);
check('成功页把令牌明文给了一次', str_contains($registered['body'], '令牌'));

echo "\n六、关掉开关就不需要验证码\n";

Settings::put('register.need_verify', false);
$app2 = probe_app_start();
$port2 = $app2['port'];
$jar2 = probe_test_temp('regcode-cookies2', '.txt');

$page2 = probe_http($port2, 'GET', '/register', [], $jar2);
check('注册页不再出现验证码框', !str_contains($page2['body'], 'name="email_code"'));
check('也不再出现「获取验证码」按钮', !str_contains($page2['body'], 'id="code-btn"'));

$csrf2 = probe_csrf($port2, '/register', $jar2);
$free = probe_http($port2, 'POST', '/register', [
    '_csrf' => $csrf2, 'email' => 'a6@example.com', 'password' => $password, 'password2' => $password,
], $jar2);
check('无验证码也能注册成功', str_contains($free['body'], '注册成功'), mb_substr($free['body'], 0, 200));
check('账号已创建', User::findByEmail('a6@example.com') !== null);

exit(probe_test_report());
