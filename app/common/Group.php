<?php
/**
 * 分组（上游线路的归类）
 *
 * ═══ 分组到底解决什么问题 ═══
 *
 * 一个站点会接多条上游线路，而它们的性质完全不同：
 *
 *   · 免费共享线路  —— 不花钱，所有用户都能用
 *   · 高速稳定专线  —— **站长在花钱**（16 元 / 74 元的额度），临时免费给用户体验
 *   · 付费线路      —— 站长花钱、用户也花钱
 *
 * 不分开的话，站长的三个问题都答不出来：
 *   ① 模型广场上「哪条线是免费的、哪条线在烧我的钱」？
 *   ② 用户把额度跑光了，是**哪条线**的额度、要去找谁充值？
 *   ③ 「临时免费」怎么结束？没有「这条线」这个单位，就没法整条线下架。
 *
 * ═══ 为什么是「挂在渠道上」而不是另建一张分组-模型表 ═══
 *
 * 本站已经有「渠道（上游线路 + 密钥池 + 模型清单）」和「定价」两套东西。
 * 再单独做一张「分组-模型」表，同一件事就有三处描述（渠道清单、定价、分组清单），
 * 迟早会对不上。所以分组只做**一个维度**：
 *   · `channels.group_id` —— 这条线路属于哪个分组（模型归谁由渠道清单决定）
 *   · `pricing.group_id`  —— 这个模型的计价口径属于哪个分组（决定收不收费）
 *   · `tokens.groups`     —— 这把令牌能访问哪些分组
 *
 * ═══ cost_mode 与 price_mode ═══
 *
 * 「临时免费」用一个字段表达不了：专线是**站长花钱、用户不花钱**。两个维度各管一头：
 *   · cost_mode  = free / paid   上游要不要花钱（运维视角）
 *   · price_mode = free / priced 对用户收不收费（用户视角）
 */

declare(strict_types=1);

namespace app\common;

use Throwable;

final class Group
{
    /** 上游不花钱 */
    public const COST_FREE = 'free';

    /** 上游花钱（要盯额度） */
    public const COST_PAID = 'paid';

    /** 对用户免费（售价按 0 计，成本照记） */
    public const PRICE_FREE = 'free';

    /** 对用户按定价收费 */
    public const PRICE_PRICED = 'priced';

    /** 内置分组代号（迁移时建的四个） */
    public const CODE_FREE = 'free';
    public const CODE_PAID = 'paid';
    public const CODE_SILICONFLOW = 'siliconflow';
    public const CODE_TIERFLOW = 'tierflow';

    /** 进程内缓存存活秒数（与 Settings 同量级） */
    private const CACHE_TTL = 5;

    /** @var array<int, array<string, mixed>>|null */
    private static ?array $cache = null;

    private static int $cacheAt = 0;

    /** 「当前可用分组」的另一份缓存（它比分组表多算了额度是否还有） */
    /** @var array<int, int>|null */
    private static ?array $liveCache = null;

    private static int $liveCacheAt = 0;

    /**
     * 全部分组（按 sort 升序）。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function all(): array
    {
        if (self::$cache !== null && (time() - self::$cacheAt) < self::CACHE_TTL) {
            return self::$cache;
        }

        try {
            $rows = Db::select('SELECT * FROM groups ORDER BY sort ASC, id ASC');
        } catch (Throwable) {
            // 表还没建好（首次启动时序）时不能把转发链路带崩
            $rows = [];
        }

        self::$cache = $rows;
        self::$cacheAt = time();

        return $rows;
    }

    /**
     * 对外可见的分组（模型广场用）：启用 + visible。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function visible(): array
    {
        return array_values(array_filter(
            self::all(),
            static fn (array $g): bool => (int) $g['status'] === 1 && (int) $g['visible'] === 1
        ));
    }

    /** @return array<string, mixed>|null */
    public static function find(int $id): ?array
    {
        foreach (self::all() as $group) {
            if ((int) $group['id'] === $id) {
                return $group;
            }
        }

        return null;
    }

    /** @return array<string, mixed>|null */
    public static function findByCode(string $code): ?array
    {
        foreach (self::all() as $group) {
            if ((string) $group['code'] === $code) {
                return $group;
            }
        }

        return null;
    }

    public static function idOfCode(string $code): int
    {
        return (int) (self::findByCode($code)['id'] ?? 0);
    }

    public static function codeOfId(int $id): string
    {
        return (string) (self::find($id)['code'] ?? '');
    }

    public static function labelOfId(int $id): string
    {
        $group = self::find($id);

        return $group === null ? '' : (string) $group['label'];
    }

    /**
     * 代号 → 展示名 的对照表。
     *
     * 令牌、渠道、定价里记的都是代号（稳定、可分享），但页面上必须显示中文名。
     * 每个页面各自查一次分组表也做得到，但那样「找不到代号时显示什么」
     * 就会各写一套，迟早出现同一个代号在三个页面显示三种东西。
     *
     * @return array<string, string>
     */
    public static function labelMap(): array
    {
        $map = [];

        foreach (self::all() as $group) {
            $map[(string) $group['code']] = (string) $group['label'];
        }

        return $map;
    }

    /**
     * 对新令牌默认可见的分组代号。
     *
     * 为什么要有这个「默认可见」标志，而不是在令牌默认值里写死一串代号：
     * 库里已经有几十把令牌了，每加一条新线路都要回头改几十把令牌的默认值，
     * 必然会漏。改成「留空 = 跟着默认可见走」，新增线路只要打开它自己的开关。
     *
     * @return array<int, string>
     */
    public static function defaultVisibleCodes(): array
    {
        $codes = [];

        foreach (self::all() as $group) {
            if ((int) $group['status'] === 1 && (int) $group['default_visible'] === 1) {
                $codes[] = (string) $group['code'];
            }
        }

        return $codes;
    }

    /**
     * 这把令牌能访问哪些分组（返回 code 列表）。
     *
     * 规则只有一条：**令牌自己填了就按它填的算，没填就用「默认可见」那一组**。
     * 不叠加、不做交集 —— 规则越简单，出问题时越好查。
     *
     * @param array<string, mixed>|null $token
     * @return array<int, string>
     */
    public static function codesOfToken(?array $token): array
    {
        $raw = trim((string) ($token['groups'] ?? ''));

        if ($raw === '') {
            return self::defaultVisibleCodes();
        }

        $codes = [];
        foreach (preg_split('/[\s,]+/', $raw) ?: [] as $code) {
            $code = trim($code);
            if ($code !== '' && self::findByCode($code) !== null) {
                $codes[$code] = true;
            }
        }

        // 令牌填的全是已删除的分组：视为「没有任何分组」，而不是悄悄放开成默认值 ——
        // 后者会让「我把分组删了」变成「所有令牌都能用」这种危险的反向结果
        return array_keys($codes);
    }

    /**
     * 这把令牌能访问哪些分组（返回 id 列表）。
     *
     * @param array<string, mixed>|null $token
     * @return array<int, int>
     */
    public static function idsOfToken(?array $token): array
    {
        $ids = [];

        foreach (self::codesOfToken($token) as $code) {
            $id = self::idOfCode($code);
            if ($id > 0) {
                $ids[] = $id;
            }
        }

        return $ids;
    }

    /**
     * 令牌能不能访问这个分组。
     *
     * @param array<string, mixed>|null $token
     */
    public static function canAccessToken(?array $token, int $groupId): bool
    {
        if ($groupId <= 0) {
            // 没挂分组的（历史数据）一律放行，行为与升级前一致
            return true;
        }

        return in_array($groupId, self::idsOfToken($token), true);
    }

    /**
     * 这个分组对用户是不是免费（售价按 0 计、成本照记）。
     *
     * 没找到分组时返回 false —— 宁可多收一次费（站长能看见），
     * 也不要因为一次查库失败就白送。
     */
    public static function isFreeToUser(int $groupId): bool
    {
        $group = self::find($groupId);

        return $group !== null && (string) $group['price_mode'] === self::PRICE_FREE;
    }

    /**
     * 这个分组要不要盯额度（上游花钱）。
     */
    public static function needsBudgetWatch(int $groupId): bool
    {
        $group = self::find($groupId);

        return $group !== null && (string) $group['cost_mode'] === self::COST_PAID;
    }

    /**
     * 当前**实际可用**的分组 id 列表。
     *
     * 「可用」= 启用 + （花钱的线路）额度还没用完。这条判断同时管两件事：
     *   · 调用：额度用完的线路不再被选路（用户拿到明确的一句话，而不是上游 4xx）
     *   · 展示：模型广场把它下架（不再列出用户点不通的模型）
     *
     * ═══ 为什么把「额度耗尽」做成整条线路下架，而不是留在那儿报错 ═══
     *
     * 站长的原话是「余额用完之后自动将密钥关闭使用并下架掉」。
     * 两者缺一不可：只停用密钥，用户仍能看到模型、点进去、拿到 4xx 或超时 ——
     * 那是最差的体验（既没服务，又让人以为是坏了）。整条线下架则是一次干净的动作。
     *
     * 结果按 5 秒进程内缓存：这条判断每个请求都要用（选路 + 列表），
     * 而它要扫全体渠道与密钥 —— 不缓存会给热路径加一堆查询。
     *
     * @return array<int, int>
     */
    public static function liveIds(): array
    {
        if (self::$liveCache !== null && (time() - self::$liveCacheAt) < self::CACHE_TTL) {
            return self::$liveCache;
        }

        $ids = [];

        foreach (self::all() as $group) {
            if ((int) $group['status'] !== 1) {
                continue;
            }

            $id = (int) $group['id'];

            // 只有「花站长钱」的线路才需要盯额度：免费额度的线路没有额度概念，
            // 不能因为这一功能被误停
            if ((string) $group['cost_mode'] !== self::COST_PAID || self::channelBudgetAvailable($id)) {
                $ids[] = $id;
            }
        }

        self::$liveCache = $ids;
        self::$liveCacheAt = time();

        return $ids;
    }

    public static function isLive(int $groupId): bool
    {
        // 未分组的历史数据一律放行（与 canAccessToken 同一口径）
        return $groupId <= 0 || in_array($groupId, self::liveIds(), true);
    }

    /**
     * 这个分组旗下还有「额度没用完的可用密钥」吗。
     *
     * 没有密钥池的渠道（单 Key 模式）视为可用：那种模式下额度由上游自己管，
     * 我们无从记账，不能因此把它当成耗尽。
     */
    private static function channelBudgetAvailable(int $groupId): bool
    {
        foreach (Channel::all() as $channel) {
            if ((int) ($channel['group_id'] ?? 0) !== $groupId) {
                continue;
            }

            if ((int) $channel['status'] !== Channel::STATUS_ENABLED) {
                continue;
            }

            $keys = ChannelKey::allForChannel((int) $channel['id']);

            if ($keys === []) {
                return true;
            }

            foreach ($keys as $key) {
                if ((int) $key['status'] === ChannelKey::STATUS_ENABLED && !ChannelKey::exhausted($key)) {
                    return true;
                }
            }
        }

        return false;
    }

    /**
     * 分组代号是否合法（后台表单校验用）。
     */
    public static function isValidCode(string $code): bool
    {
        return preg_match('/^[a-z][a-z0-9_]{1,31}$/', $code) === 1;
    }

    /**
     * 新建分组。
     *
     * @param array<string, mixed> $data
     * @return array{ok:bool, message:string, id:int}
     */
    public static function create(array $data): array
    {
        $code = trim((string) ($data['code'] ?? ''));
        $label = trim((string) ($data['label'] ?? ''));

        if (!self::isValidCode($code)) {
            return ['ok' => false, 'message' => '代号只能用小写字母、数字与下划线，且以字母开头（例如 express_a）', 'id' => 0];
        }

        if ($label === '') {
            return ['ok' => false, 'message' => '展示名不能为空', 'id' => 0];
        }

        if (self::findByCode($code) !== null) {
            return ['ok' => false, 'message' => "代理号「{$code}」已经有一个分组在用了", 'id' => 0];
        }

        $now = time();

        try {
            Db::execute(
                'INSERT INTO groups (code, label, description, cost_mode, price_mode, visible, default_visible, sort, status, created_at, updated_at)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
                [
                    $code,
                    mb_substr($label, 0, 64),
                    mb_substr(trim((string) ($data['description'] ?? '')), 0, 255),
                    (string) ($data['cost_mode'] ?? self::COST_FREE) === self::COST_PAID ? self::COST_PAID : self::COST_FREE,
                    (string) ($data['price_mode'] ?? self::PRICE_PRICED) === self::PRICE_FREE ? self::PRICE_FREE : self::PRICE_PRICED,
                    !empty($data['visible']) ? 1 : 0,
                    !empty($data['default_visible']) ? 1 : 0,
                    (int) ($data['sort'] ?? 0),
                    !empty($data['status']) ? 1 : 0,
                    $now,
                    $now,
                ]
            );
        } catch (Throwable $e) {
            return ['ok' => false, 'message' => '建分组失败：' . $e->getMessage(), 'id' => 0];
        }

        self::forget();

        return ['ok' => true, 'message' => "分组「{$label}」已创建", 'id' => (int) Db::pdo()->lastInsertId()];
    }

    /**
     * 修改分组（不动 code —— 令牌里记的是它，改了等于把令牌的分组权限全弄丢）。
     *
     * @param array<string, mixed> $data
     * @return array{ok:bool, message:string}
     */
    public static function update(int $id, array $data): array
    {
        $group = self::find($id);

        if ($group === null) {
            return ['ok' => false, 'message' => '分组不存在'];
        }

        $label = trim((string) ($data['label'] ?? ''));
        if ($label === '') {
            return ['ok' => false, 'message' => '展示名不能为空'];
        }

        try {
            Db::execute(
                'UPDATE groups SET label = ?, description = ?, cost_mode = ?, price_mode = ?, visible = ?, default_visible = ?, sort = ?, status = ?, updated_at = ?
                 WHERE id = ?',
                [
                    mb_substr($label, 0, 64),
                    mb_substr(trim((string) ($data['description'] ?? '')), 0, 255),
                    (string) ($data['cost_mode'] ?? self::COST_FREE) === self::COST_PAID ? self::COST_PAID : self::COST_FREE,
                    (string) ($data['price_mode'] ?? self::PRICE_PRICED) === self::PRICE_FREE ? self::PRICE_FREE : self::PRICE_PRICED,
                    !empty($data['visible']) ? 1 : 0,
                    !empty($data['default_visible']) ? 1 : 0,
                    (int) ($data['sort'] ?? 0),
                    !empty($data['status']) ? 1 : 0,
                    time(),
                    $id,
                ]
            );
        } catch (Throwable $e) {
            return ['ok' => false, 'message' => '保存失败：' . $e->getMessage()];
        }

        self::forget();

        return ['ok' => true, 'message' => "分组「{$label}」已保存"];
    }

    /**
     * 删除分组。
     *
     * 只允许删**空分组**（没有渠道也没有定价挂在它下面）：删掉一个还在供货的分组，
     * 那些渠道的模型会瞬间失去归属，用户那边的调用会以「分组权限不足」被拒 ——
     * 这是不可接受的副作用，所以宁可拒绝删除并提示先搬走。
     *
     * @return array{ok:bool, message:string}
     */
    public static function delete(int $id): array
    {
        $group = self::find($id);

        if ($group === null) {
            return ['ok' => false, 'message' => '分组不存在'];
        }

        $channels = (int) (Db::selectOne('SELECT COUNT(*) AS c FROM channels WHERE group_id = ?', [$id])['c'] ?? 0);
        $prices = (int) (Db::selectOne('SELECT COUNT(*) AS c FROM pricing WHERE group_id = ?', [$id])['c'] ?? 0);

        if ($channels > 0 || $prices > 0) {
            return [
                'ok' => false,
                'message' => "这个分组下还有 {$channels} 条渠道、{$prices} 条定价，不能删。"
                    . '请先把它们改到别的分组（或删掉），再来删这个空分组',
            ];
        }

        try {
            Db::execute('DELETE FROM groups WHERE id = ?', [$id]);
        } catch (Throwable $e) {
            return ['ok' => false, 'message' => '删除失败：' . $e->getMessage()];
        }

        self::forget();

        return ['ok' => true, 'message' => "分组「{$group['label']}」已删除"];
    }

    /**
     * 分组一览（后台列表用）：每个分组带渠道数、模型数、定价数，
     * 以及「这条线是不是在烧钱」的汇总。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function overview(): array
    {
        $rows = [];

        foreach (self::all() as $group) {
            $id = (int) $group['id'];

            $channelCount = 0;
            $models = [];
            foreach (Channel::all() as $channel) {
                if ((int) ($channel['group_id'] ?? 0) !== $id) {
                    continue;
                }
                $channelCount++;
                foreach (Channel::modelsOf($channel) as $model) {
                    $models[(string) $model] = true;
                }
            }

            $priceCount = (int) (Db::selectOne('SELECT COUNT(*) AS c FROM pricing WHERE group_id = ?', [$id])['c'] ?? 0);

            $rows[] = [
                'id' => $id,
                'code' => (string) $group['code'],
                'label' => (string) $group['label'],
                'description' => (string) ($group['description'] ?? ''),
                'costMode' => (string) $group['cost_mode'],
                'priceMode' => (string) $group['price_mode'],
                'visible' => (int) $group['visible'] === 1,
                'defaultVisible' => (int) $group['default_visible'] === 1,
                'enabled' => (int) $group['status'] === 1,
                'sort' => (int) $group['sort'],
                'channelCount' => $channelCount,
                'modelCount' => count($models),
                'priceCount' => $priceCount,
            ];
        }

        return $rows;
    }

    /** 清掉进程内缓存（后台改完分组立刻生效） */
    public static function forget(): void
    {
        self::$cache = null;
        self::$cacheAt = 0;
        self::$liveCache = null;
        self::$liveCacheAt = 0;
    }
}
