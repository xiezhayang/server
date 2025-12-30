using System.Collections;
using System.Collections.Generic;
using System.IO.MemoryMappedFiles;
using System.Runtime.InteropServices;
using UnityEngine;
using System;

// 共享内存数据结构（与Go端保持一致）
[StructLayout(LayoutKind.Sequential, Pack = 1)]
public  struct SharedMemoryData
{
    public uint PlayerID;
    public float PositionX;
    public float PositionY;
    public float VelocityX;
    public float VelocityY;
    public float InputX;
    public float InputY;
    public uint IsJumping;
    public uint IsConnected;
    public uint FrameCounter;
}

public class PhysicsController : MonoBehaviour
{
    [Header("共享内存设置")]
    public string sharedMemoryName = "UnityGoSharedMemory";
    public int sharedMemorySize = 1024;

    [Header("协程设置")]
    [SerializeField] private float inputUpdateRate = 60f;  // 输入更新频率 (Hz)
    [SerializeField] private float physicsUpdateRate = 60f; // 物理更新频率 (Hz)

    [Header("调试信息")]
    [SerializeField] private bool showDebugInfo = true;

    private MemoryMappedFile sharedMemory;
    private MemoryMappedViewAccessor accessor;
    private SharedMemoryData sharedData;
    private Rigidbody2D rb;

    // 协程控制字段
    private bool isGrounded = true;  // ✅ 添加缺失的变量
    private object sharedMemoryLock = new object();
    
    // 性能计数
    private int inputFrameCount = 0;
    private int physicsFrameCount = 0;
    private float lastDebugTime = 0f;

    void Start()
    {
        rb = GetComponent<Rigidbody2D>();
        if (rb != null)
        {
            rb.isKinematic = true;  // 禁用本地物理，完全依赖Go
        }

        // 初始化共享内存
        InitializeSharedMemory();

        // 启动协程（移除原有的Update方法）
        StartCoroutine(InputCoroutine());
        StartCoroutine(PhysicsCoroutine());
        StartCoroutine(DebugCoroutine());

        Debug.Log("PhysicsController 协程版本初始化完成");
    }

    void InitializeSharedMemory()
    {
        try
        {
            sharedMemory = MemoryMappedFile.CreateOrOpen(sharedMemoryName, sharedMemorySize);
            accessor = sharedMemory.CreateViewAccessor();

            // 初始化共享内存数据
            sharedData = new SharedMemoryData
            {
                PlayerID = 1u,  
                PositionX = transform.position.x,
                PositionY = transform.position.y,
                IsConnected = 1u,
                FrameCounter = 0
            };
            WriteToSharedMemory();
            Debug.Log($"共享内存 {sharedMemoryName} 初始化成功");
        }
        catch (System.Exception e)
        {
            Debug.LogError($"共享内存初始化失败: {e.Message}");
        }
    }

    // 输入协程 - 专门处理用户输入
    IEnumerator InputCoroutine()
    {
        float interval = 1f / inputUpdateRate;
        WaitForSeconds wait = new WaitForSeconds(interval);

        Debug.Log($"输入协程启动，更新频率: {inputUpdateRate}Hz");

        while (true)
        {
            // 读取输入
            HandleInput();

            // 写入共享内存（线程安全）
            lock (sharedMemoryLock)
            {
                WriteInputToSharedMemory();
            }

            inputFrameCount++;

            yield return wait;
        }
    }

    // 物理协程 - 专门处理物理更新
    IEnumerator PhysicsCoroutine()
    {
        float interval = 1f / physicsUpdateRate;
        WaitForSeconds wait = new WaitForSeconds(interval);

        Debug.Log($"物理协程启动，更新频率: {physicsUpdateRate}Hz");

        while (true)
        {
            // 读取共享内存（线程安全）
            lock (sharedMemoryLock)
            {
                ReadPhysicsFromSharedMemory();
            }

            // 更新Unity物体
            UpdateTransform();

            physicsFrameCount++;

            yield return wait;
        }
    }

    // 调试协程 - 显示性能信息
    IEnumerator DebugCoroutine()
    {
        WaitForSeconds wait = new WaitForSeconds(1f);

        while (true)
        {
            if (showDebugInfo && Time.time - lastDebugTime >= 1f)
            {
                Debug.Log($"输入帧率: {inputFrameCount}/s, 物理帧率: {physicsFrameCount}/s, Go帧计数: {sharedData.FrameCounter}");
                inputFrameCount = 0;
                physicsFrameCount = 0;
                lastDebugTime = Time.time;
            }
            yield return wait;
        }
    }

    void HandleInput()
    {
        // 读取键盘输入
        float horizontal = Input.GetAxis("Horizontal");
        float vertical = Input.GetAxis("Vertical");
        bool jump = Input.GetKeyDown(KeyCode.Space);

        // 更新共享数据（不直接写入内存）
        sharedData.InputX = horizontal;
        sharedData.InputY = vertical;
        sharedData.IsJumping = jump ? 1u : 0u;
    }

    void WriteInputToSharedMemory()
    {
        try
        {
            accessor.Write(0, ref sharedData);
           // sharedData.FrameCounter++;
        }
        catch (System.Exception e)
        {
            Debug.LogError($"写入共享内存失败: {e.Message}");
        }
    }

    void ReadPhysicsFromSharedMemory()
    {
        try
        {
            accessor.Read(0, out sharedData);
        }
        catch (System.Exception e)
        {
            Debug.LogError($"读取共享内存失败: {e.Message}");
        }
    }

    void UpdateTransform()
    {
        // 完全使用Go物理引擎的计算结果
        transform.position = new Vector3(sharedData.PositionX, sharedData.PositionY, 0);

        // 可选：更新Rigidbody2D的速度用于显示
        if (rb != null)
        {
            rb.velocity = new Vector2(sharedData.VelocityX, sharedData.VelocityY);
        }
    }

    // 碰撞检测（如果需要）
    void OnCollisionEnter2D(Collision2D collision)
    {
        if (collision.gameObject.CompareTag("Ground"))
        {
            isGrounded = true;  // ✅ 现在这个变量存在了
        }
    }

    void OnApplicationQuit()
    {
        // 停止所有协程
        StopAllCoroutines();

        // 清理共享内存
        if (accessor != null)
        {
            lock (sharedMemoryLock)
            {
                sharedData.IsConnected = 0u;
                WriteToSharedMemory();
                accessor.Dispose();
            }
        }
        if (sharedMemory != null)
        {
            sharedMemory.Dispose();
        }
        Debug.Log("PhysicsController 协程版本清理完成");
    }

    void WriteToSharedMemory()
    {
        try
        {
            accessor.Write(0, ref sharedData);
        }
        catch (System.Exception e)
        {
            Debug.LogError($"写入共享内存失败: {e.Message}");
        }
    }

    // 在Inspector中显示实时信息
    void OnGUI()
    {
        if (showDebugInfo)
        {
            GUILayout.BeginArea(new Rect(10, 10, 300, 200));
            GUILayout.Label($"PhysicsController - 协程版本");
            GUILayout.Label($"输入帧率: {inputFrameCount}/s");
            GUILayout.Label($"物理帧率: {physicsFrameCount}/s");
            GUILayout.Label($"Go帧计数: {sharedData.FrameCounter}");
            GUILayout.Label($"位置: ({sharedData.PositionX:F2}, {sharedData.PositionY:F2})");
            GUILayout.Label($"速度: ({sharedData.VelocityX:F2}, {sharedData.VelocityY:F2})");
            GUILayout.Label($"输入: ({sharedData.InputX:F2}, {sharedData.InputY:F2})");
            GUILayout.Label($"接地状态: {isGrounded}");
            GUILayout.EndArea();
        }
    }
}
