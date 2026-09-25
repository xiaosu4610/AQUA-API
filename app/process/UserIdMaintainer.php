<?php
/**
 * 用户编号维护进程
 *
 * 只做一件事：每隔 `users.compact_interval_days` 天（默认 3 天）
 * 把用户编号重排成连续的 1..N，把注销留下的空位补上。
 *
 * ═══ 为什么需要它 ═══
 *
 * 用户注销后那一行会真的被删掉，编号就出现空位（1、3、5…）。
 * 空位本身不致命，但用户和站长都会拿编号当「第几个用户」看，
 * 空位会让人以为系统丢了数据；站长也常常要手工去改数据库。
 * 交给一个独立的定时进程，就不用任何人惦记这件事。
 *
 * ═══ 为什么单独一个进程 ═══
 *
 * `count => 1`：全集群只有这一个实例在跑。
 * 放在 HTTP 工作进程里做（例如在某次请求后顺手检查）就会出现
 * 「多个进程同时决定动手」的竞争 —— 而重排是搬主键，不能并发做。
 *
 * 它不监听任何端口，只是挂着两个定时器，因此占用的资源可以忽略。
 *
 * ═══ 为什么要等「安静」才动手 ═══
 *
 * 重排之后编号 5 可能已经换成了另一个人。如果此刻正好有请求在飞
 * （它已经拿到编号 5，正要去扣费），这笔钱就会记到新主人账上。
 * 所以执行前必须确认近 5 分钟没有调用流量（见 User::canCompactNow）。
 */

declare(strict_types=1);

namespace app\process;

use app\common\Schema;
use app\common\Settings;
use app\common\User;
use support\Log;
use Throwable;
use Workerman\Timer;
use Workerman\Worker;

class UserIdMaintainer extends Worker
{
    /** 巡检间隔：一小时看一次，真正的「够不够 3 天」由 runOnce 内部判断 */
    private const CHECK_INTERVAL = 3600;

    /** 启动后延迟多久做第一次巡检（秒）——避开启动瞬间的建表与迁移 */
    private const FIRST_DELAY = 120;

    public function onWorkerStart(): void
    {
        // 自定义进程不一定享受 bootstrap 链，这里自己确保一次表结构
        // （Schema::ensure 是幂等的，重复执行无害）
        try {
            Schema::ensure();
        } catch (Throwable $e) {
            Log::error('用户编号维护进程：数据库尚未就绪，将在下次巡检时重试 —— ' . $e->getMessage());
        }

        Timer::add(self::FIRST_DELAY, [$this, 'tick'], [], false);
        Timer::add(self::CHECK_INTERVAL, [$this, 'tick'], [], true);
    }

    /**
     * 定时器的入口。
     *
     * 异常必须在这里兜住：维护任务出问题不能让整个服务崩掉，
     * 记一条日志、下一轮再说就够。
     */
    public function tick(): void
    {
        try {
            $this->runOnce();
        } catch (Throwable $e) {
            Log::error('用户编号补位失败：' . $e->getMessage());
        }
    }

    private function runOnce(): void
    {
        if (!Settings::bool('users.auto_compact_ids', true)) {
            return;
        }

        $days = max(1, Settings::int('users.compact_interval_days', 3));
        $last = Settings::int('users.last_compacted_at', 0);

        if ($last > 0 && (time() - $last) < $days * 86400) {
            return;
        }

        if (!User::needsCompact()) {
            // 没有空位也要记一次时间：否则每个小时都会重新算一遍
            Settings::put('users.last_compacted_at', time());

            return;
        }

        if (!User::canCompactNow()) {
            // 有在途流量：这一轮先不动，留给下一轮
            Log::info('用户编号补位：近 5 分钟内有调用流量，本轮跳过');

            return;
        }

        $result = User::compactIds();

        Log::info(sprintf(
            '用户编号补位完成：共 %d 个账号，移动 %d 个（旧登录态已全部失效）',
            $result['total'],
            $result['moved']
        ));
    }
}
