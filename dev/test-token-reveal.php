<?php
/**
 * 令牌「随时复制」（可回显）的测试。
 *
 * ═══ 这件事为什么必须测 ═══
 *
 * 1. **安全边界变了，必须确认它只变了该变的那一点**
 *    令牌原来只存 SHA-256 哈希，现在多存一份可逆副本。
 *    要守住的是：**鉴权路径一点没变**（仍然按哈希比对），
 *    副本只在展示时用；否则「为了方便」就会顺带把鉴权也搞歪。
 *
 * 2. **开关关掉就必须真的不存**
 *    「关了但还在存」是很典型的一类 bug：页面看着关了，
 *    库里照样新增可逆副本 —— 站长的安全判断被悄悄推翻，
 *    而且不看数据库根本发现不了。
 *
 * 3. **关开关不会让存量副本消失**
 *    所以还有「清空已保存的副本」这个动作。测它，是为了确认
 *    清的是副本、不是令牌本身（清完用户手上的令牌还必须能用）。
 *
 * 4. **文案不能说反**
 *    关了却说「以后还能再看到」会让用户真的丢掉令牌。三种状态
 *    （开着有副本 / 开着但没副本 / 关着）页面都得说对。
 *
 * 用法：php dev/test-token-reveal.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Crypto;
use app\common\Db;
use app\common\Schema;
use app\common\Settings;
use app\common\User;
use app\common\UserToken;

$dbFile = probe_test_temp('token-reveal-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('reveal-pass-1234');

echo "一、前提：本站能加密（不能加密时「回显」是无从谈起的）\n";

check('已配置 APP_KEY', Crypto::isConfigured(), '没有 APP_KEY 就存不了可逆副本');
check('默认开着「随时复制」', UserToken::revealEnabled(), '默认值不对');

echo "\n二、开关开着：新建的令牌可以再取回明文\n";

$user = User::register('reveal-1@example.com', 'user-pass-1234', '127.0.0.1', false, 0.0);
check('建出测试用户', $user['ok'] === true, (string) ($user['message'] ?? ''));
$uid = (int) $user['id'];

$created = UserToken::create($uid, '第一把');
$plain = $created['plain'];
$row = UserToken::find((int) $created['id']);

check('令牌明文带本站前缀', str_starts_with($plain, UserToken::PREFIX), $plain);
check('库里存了可回显副本', trim((string) $row['key_enc']) !== '', 'key_enc 是空的');
check('副本不是明文（是加密后的串）', !str_contains((string) $row['key_enc'], $plain));
check('取回明文与创建时一致', UserToken::plainOf($row) === $plain, UserToken::plainOf($row));
check('掩码形态正确', UserToken::mask($plain) === mb_substr($plain, 0, 12) . '…' . mb_substr($plain, -4));

// ── 这一条是重点：鉴权路径必须仍然是哈希 ──
$byPlain = UserToken::findByPlain($plain);
check('鉴权仍按哈希能找到这把令牌', $byPlain !== null && (int) $byPlain['id'] === (int) $created['id']);
check('鉴权不看副本：把副本清掉也能按明文找到', (static function () use ($plain, $created): bool {
    Db::execute('UPDATE tokens SET key_enc = NULL WHERE id = ?', [(int) $created['id']]);

    return UserToken::findByPlain($plain) !== null;
})());
// 还原副本，后面还要用
Db::execute(
    'UPDATE tokens SET key_enc = ? WHERE id = ?',
    [Crypto::encrypt($plain), (int) $created['id']]
);

echo "\n三、开关关掉：只存哈希，明文再也取不回来\n";

Settings::put('security.token_reveal', false);
Settings::forget();

check('开关生效（revealEnabled 为假）', UserToken::revealEnabled() === false);

$off = UserToken::create($uid, '关着建的');
$offRow = UserToken::find((int) $off['id']);

check('关掉后新建的令牌不存副本', ($offRow['key_enc'] ?? null) === null, var_export($offRow['key_enc'] ?? null, true));
check('关掉后取不到明文', UserToken::plainOf($offRow) === '', '居然还能取到明文');
check('关掉后老令牌（本来有副本）也取不到明文', UserToken::plainOf(UserToken::find((int) $created['id'])) === '');
check('关掉不影响调用：明文按哈希照样能查到', UserToken::findByPlain($off['plain']) !== null);
check('副本仍在库里躺着（关开关不会让它消失）', UserToken::countRevealable() === 1, (string) UserToken::countRevealable());

echo "\n四、清空已保存的副本\n";

$cleared = UserToken::purgePlaintext();
check('清掉 1 条', $cleared === 1, (string) $cleared);
check('清完就没有可回显的令牌了', UserToken::countRevealable() === 0, (string) UserToken::countRevealable());
check('清的是副本不是令牌：用户手上的令牌照样能用', UserToken::findByPlain($plain) !== null);

Settings::put('security.token_reveal', true);
Settings::forget();
check('重新打开开关后，新建的令牌又有副本', (static function () use ($uid): bool {
    $row = UserToken::find((int) UserToken::create($uid, '重开之后')['id']);

    return trim((string) $row['key_enc']) !== '';
})());

echo "\n五、控制台页面：用户看到的是完整令牌 + 复制按钮\n";

// 上面第四步把已存的副本清空了，这里先建一把带副本的，作为「用户能复制到的那把」
$visible = UserToken::create($uid, '待复制')['plain'];

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('token-reveal-cookies', '.txt');

$csrf = probe_csrf($port, '/login', $jar);
check('取到登录页 CSRF', $csrf !== '');
$login = probe_http($port, 'POST', '/login', [
    '_csrf' => $csrf,
    'email' => 'reveal-1@example.com',
    'password' => 'user-pass-1234',
], $jar);
check('用户登录成功', $login['status'] === 302 && $login['location'] === '/console', $login['location']);

$console = probe_http($port, 'GET', '/console', [], $jar);
$body = $console['body'];
check('控制台可访问', $console['status'] === 200, (string) $console['status']);
check('页面里出现了完整的令牌', str_contains($body, $visible), '没有完整令牌');
check('令牌旁边有复制按钮', preg_match('/data-copy="' . preg_quote($visible, '/') . '"/', $body) === 1);
check('说明里写的是「随时复制」', str_contains($body, '随时复制'));
check('没有出现「只显示一次」这种过时说法', !str_contains($body, '令牌只在创建时显示一次'));

echo "\n六、开关关掉后，控制台如实说清原因\n";

Settings::put('security.token_reveal', false);
Settings::forget();
sleep(6);   // 配置有 5 秒进程内缓存，等应用进程也读到新值

$offBody = probe_http($port, 'GET', '/console', [], $jar)['body'];
check('不再显示完整令牌', !str_contains($offBody, $visible), '还显示着完整令牌');
check('显示的是掩码', str_contains($offBody, UserToken::mask($visible)));
check('说清了是站长关掉的（而不是让人以为是系统坏了）', str_contains($offBody, '站长关闭了「随时复制令牌」'));

Settings::put('security.token_reveal', true);
Settings::forget();
sleep(6);

echo "\n七、后台：卡片与「清空副本」动作\n";

$adminJar = probe_test_temp('token-reveal-admin', '.txt');
check('登录后台', probe_login($port, $adminJar, 'reveal-pass-1234'));
$adminCsrf = probe_csrf($port, '/admin/settings', $adminJar);

$settingsPage = probe_http($port, 'GET', '/admin/settings', [], $adminJar)['body'];
check('配置页有「令牌「随时复制」」卡片', str_contains($settingsPage, '令牌「随时复制」'));
check('卡片写明了代价（库 + APP_KEY 同时泄露）', str_contains($settingsPage, 'APP_KEY 同时泄露'));
check('卡片显示当前存着多少把副本', str_contains($settingsPage, '把令牌保存着可回显副本'));
check('有「清空已保存的令牌副本」按钮', str_contains($settingsPage, '清空已保存的令牌副本'));
check('开关本身仍在「安全」组里', str_contains($settingsPage, '允许用户随时复制自己的令牌'));

$before = UserToken::countRevealable();
$noCsrf = probe_http($port, 'POST', '/admin/settings/token-reveal/purge', [], $adminJar);
check('没有 CSRF → 被拒', $noCsrf['status'] === 302 && $noCsrf['location'] === '/admin/settings', $noCsrf['location']);
Settings::forget();
check('被拒时副本没被清掉', UserToken::countRevealable() === $before, (string) UserToken::countRevealable());

$purge = probe_http($port, 'POST', '/admin/settings/token-reveal/purge', ['_csrf' => $adminCsrf], $adminJar);
check('清空动作执行成功并跳回配置页', $purge['status'] === 302 && $purge['location'] === '/admin/settings', $purge['location']);
Settings::forget();
check('副本确实被清空了', UserToken::countRevealable() === 0, (string) UserToken::countRevealable());
check('令牌本身没被删（用户的令牌照样能查到）', UserToken::findByPlain($plain) !== null);
check('页面提示说明了令牌不受影响', str_contains(
    probe_http($port, 'GET', '/admin/settings', [], $adminJar)['body'],
    '令牌本身照旧能用'
));

$again = probe_http($port, 'POST', '/admin/settings/token-reveal/purge', ['_csrf' => $adminCsrf], $adminJar);
check('没有副本可清时给出提示而不是报错', $again['status'] === 302);
check('提示「没有需要清除的令牌副本」', str_contains(
    probe_http($port, 'GET', '/admin/settings', [], $adminJar)['body'],
    '没有需要清除的令牌副本'
));

exit(probe_test_report());
