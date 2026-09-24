<?php
/**
 * .env 文件读写
 *
 * 用于安装程序把「引导配置」写进 .env。这里有一条必须守住的规矩：
 *
 * ⚠️ **同一个键在文件里只能出现一次**。
 *
 * 因为 dotenv 的解析规则是「第一个定义生效，后面的重复定义被忽略」。
 * 一旦出现两行 `APP_KEY=`，第二行的真实值永远不会生效 ——
 * 这个坑项目上已经踩过一次：.env 里先出现空的 `ADMIN_PASSWORD=`，
 * 后面 append 的真实密码被无视，结果管理员死活建不出来，
 * 而现象是「登录时提示后台尚未初始化」，完全指不到真正的原因。
 *
 * 所以本类**不是**简单地往文件末尾追加，而是：
 *   1. 以 .env.example 为模板（保留里面所有注释与说明）；
 *   2. 逐个键做「替换」（连被注释掉的 `#SESSION_SECURE=` 也一并替换）；
 *   3. 模板里没有的键才追加，且追加前先确认文件里确实没有同名活动行。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;

final class EnvFile
{
    /** .env 路径 */
    public static function path(): string
    {
        return base_path() . DIRECTORY_SEPARATOR . '.env';
    }

    /** 模板路径（.env.example，唯一允许入库的配置文件） */
    public static function templatePath(): string
    {
        return base_path() . DIRECTORY_SEPARATOR . '.env.example';
    }

    public static function exists(): bool
    {
        return is_file(self::path());
    }

    /**
     * 读取现有 .env 的键值（第一个定义生效，与运行时解析规则一致）。
     *
     * @return array<string, string>
     */
    public static function read(): array
    {
        if (!self::exists()) {
            return [];
        }

        $vars = [];

        foreach (file(self::path(), FILE_IGNORE_NEW_LINES | FILE_SKIP_EMPTY_LINES) ?: [] as $line) {
            $line = trim($line);
            if ($line === '' || $line[0] === '#') {
                continue;
            }

            $pos = strpos($line, '=');
            if ($pos === false) {
                continue;
            }

            $key = trim(substr($line, 0, $pos));
            if (!array_key_exists($key, $vars)) {
                $vars[$key] = trim(substr($line, $pos + 1), " \t\"'");
            }
        }

        return $vars;
    }

    /**
     * 写入（或更新）若干键。
     *
     * @param array<string, string> $values 键 => 值（值会被自动转义）
     * @throws RuntimeException 写入失败（通常是权限问题）
     */
    public static function write(array $values): void
    {
        $template = is_readable(self::templatePath())
            ? (string) file_get_contents(self::templatePath())
            : '';

        foreach ($values as $key => $value) {
            $key = trim((string) $key);
            if ($key === '') {
                continue;
            }

            $line = $key . '=' . self::escape((string) $value);

            // ① 活动行：直接替换（只替第一处，保证不会越改越多）
            $pattern = '/^' . preg_quote($key, '/') . '=.*$/m';
            if (preg_match($pattern, $template)) {
                $template = (string) preg_replace($pattern, $line, $template, 1);
                continue;
            }

            // ② 被注释掉的默认行，例如 .env.example 里的 `#SESSION_SECURE=false`。
            //    这种情况要「变成活动行」而不是再追加一行，否则同一个键会有两处
            $commented = '/^#\s*' . preg_quote($key, '/') . '=.*$/m';
            if (preg_match($commented, $template)) {
                $template = (string) preg_replace($commented, $line, $template, 1);
                continue;
            }

            // ③ 模板里完全没有这个键：追加到末尾的独立区块
            $template = rtrim($template) . "\n\n# 由安装程序写入\n" . $line . "\n";
        }

        $path = self::path();

        if (file_put_contents($path, $template) === false) {
            throw new RuntimeException('无法写入 .env，请检查项目根目录的写权限');
        }

        // .env 里迟早会有敏感信息（APP_KEY、数据库口令），
        // 因此只要写得动就顺手收紧权限。Windows 下 chmod 基本无效果，忽略即可
        @chmod($path, 0600);
    }

    /**
     * 备份现有 .env（安装程序覆盖前调用，给站长留一条退路）。
     *
     * @return string|null 备份文件路径；没有原文件时返回 null
     */
    public static function backup(): ?string
    {
        if (!self::exists()) {
            return null;
        }

        $target = self::path() . '.bak-' . date('YmdHis');

        return copy(self::path(), $target) ? $target : null;
    }

    /**
     * 值转义。
     *
     * 含空格、引号、井号的值必须加引号，否则会被解析成
     * 「值被截断」或「后面的内容被当成注释」——
     * 数据库口令里出现这两类字符是常事。
     */
    private static function escape(string $value): string
    {
        if ($value === '') {
            return '';
        }

        if (preg_match('/[\s#"\'\\\\]/', $value) === 1) {
            // 双引号包裹 + 转义内部的引号与反斜杠
            return '"' . addcslashes($value, "\\\"") . '"';
        }

        return $value;
    }
}
