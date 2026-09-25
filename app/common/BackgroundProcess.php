<?php
/**
 * 跨平台启动模型探测 CLI 子进程。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;

final class BackgroundProcess
{
    public static function startModelProbe(int $taskId): void
    {
        if ($taskId <= 0) {
            throw new RuntimeException('任务 ID 无效');
        }

        $disabled = array_filter(array_map('trim', explode(',', (string) ini_get('disable_functions'))));
        if (PHP_OS_FAMILY === 'Windows') {
            if (in_array('popen', $disabled, true)) {
                throw new RuntimeException('服务器禁止启动后台任务（popen 已禁用）');
            }
            self::startWindows($taskId);
            return;
        }

        if (in_array('proc_open', $disabled, true)) {
            throw new RuntimeException('服务器禁止启动后台任务（proc_open 已禁用）');
        }
        self::startUnix($taskId);
    }

    private static function startWindows(int $taskId): void
    {
        $php = self::phpBinary();
        $script = base_path() . DIRECTORY_SEPARATOR . 'scripts' . DIRECTORY_SEPARATOR . 'run-model-probe-task.php';
        $command = 'start "" /B ' . escapeshellarg($php) . ' ' . escapeshellarg($script) . ' ' . $taskId;
        $handle = @popen($command, 'r');
        if ($handle === false) {
            throw new RuntimeException('无法启动后台检测进程');
        }
        pclose($handle);
    }

    private static function startUnix(int $taskId): void
    {
        $php = self::phpBinary();
        $script = base_path() . DIRECTORY_SEPARATOR . 'scripts' . DIRECTORY_SEPARATOR . 'run-model-probe-task.php';
        $command = 'nohup ' . escapeshellarg($php) . ' ' . escapeshellarg($script) . ' ' . $taskId
            . ' >/dev/null 2>&1 &';
        $process = @proc_open($command, [], $pipes, base_path());
        if (!is_resource($process)) {
            throw new RuntimeException('无法启动后台检测进程');
        }
        proc_close($process);
    }

    private static function phpBinary(): string
    {
        $binary = PHP_BINARY;
        if ($binary !== '' && is_file($binary)) {
            return $binary;
        }

        return PHP_OS_FAMILY === 'Windows' ? 'php.exe' : 'php';
    }
}
