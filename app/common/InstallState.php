<?php
/**
 * 安装状态
 *
 * 负责回答一个问题：**这套系统装好了没有？**
 * 安装程序据此决定「让你继续装」还是「告诉你已经装好了、别再来了」。
 *
 * ═══ 两道判断，为什么不是一道 ═══
 *
 *   1. **锁文件**（runtime/install.lock）—— 快，一次 is_file 就够。
 *      安装成功后写入，内容含安装时间、数据库类型等，便于事后追溯。
 *
 *   2. **数据库实据** —— 慢一点，但要兜住一种情况：
 *      锁文件在 runtime/ 下，而 runtime/ 是可以被清空的（清缓存、
 *      误删目录、换机器只搬了代码）。这时锁文件没了，
 *      但库里明明有管理员和表结构 —— 只看锁文件就会把已上线的站点
 *      重新导向安装程序，那是很危险的事。
 *
 * ═══ 判断失败时倾向于「已安装」 ═══
 *
 * 数据库连不上时（比如 MySQL 临时挂了），本类返回 **true**（视为已安装）。
 * 理由：宁可让站长看到真实的服务错误，也不能把线上站点劫持到安装向导 ——
 * 后者会让人误以为数据丢了，甚至手忙脚乱地「重装」一遍。
 */

declare(strict_types=1);

namespace app\common;

use Throwable;

final class InstallState
{
    /** 锁文件路径 */
    public static function lockPath(): string
    {
        return runtime_path() . DIRECTORY_SEPARATOR . 'install.lock';
    }

    /**
     * 是否已完成安装。
     */
    public static function isInstalled(): bool
    {
        // ① 锁文件：最快、最明确的证据
        if (is_file(self::lockPath())) {
            return true;
        }

        // ② .env 都没有，那确实还没装
        if (!EnvFile::exists()) {
            return false;
        }

        // ③ 回落到数据库实据
        try {
            $hasSchema = Settings::raw('schema_version') !== null;
            $hasAdmin = Admin::find() !== null;

            if ($hasSchema && $hasAdmin) {
                return true;
            }
        } catch (Throwable) {
            // 库连不上 —— 见文件头说明：此时按「已安装」处理，
            // 让站长看到真实错误，而不是被导向安装向导
            return true;
        }

        return false;
    }

    /**
     * 记录安装完成（写锁文件）。
     *
     * @param array<string, mixed> $info 安装摘要：数据库类型、站点名等
     */
    public static function markInstalled(array $info): void
    {
        $info['installed_at'] = date('c');
        $info['app_version'] = self::appVersion();

        $dir = dirname(self::lockPath());
        if (!is_dir($dir)) {
            @mkdir($dir, 0755, true);
        }

        @file_put_contents(
            self::lockPath(),
            (string) json_encode($info, JSON_UNESCAPED_UNICODE | JSON_PRETTY_PRINT) . "\n"
        );
    }

    /**
     * 读取锁文件内容（供「已安装」页面展示，便于核对装的是哪套）。
     *
     * @return array<string, mixed>
     */
    public static function info(): array
    {
        if (!is_file(self::lockPath())) {
            return [];
        }

        $decoded = json_decode((string) file_get_contents(self::lockPath()), true);

        return is_array($decoded) ? $decoded : [];
    }

    /**
     * 当前代码版本号。
     *
     * 取 composer.json 的 version 字段；没有就退回 'dev'。
     * 不用常量写死，是为了避免「改了版本号忘了改代码」这类不一致。
     *
     * @return string
     */
    public static function appVersion(): string
    {
        $file = base_path() . DIRECTORY_SEPARATOR . 'composer.json';
        if (!is_readable($file)) {
            return 'dev';
        }

        $decoded = json_decode((string) file_get_contents($file), true);

        return is_array($decoded) && !empty($decoded['version'])
            ? (string) $decoded['version']
            : 'dev';
    }
}
