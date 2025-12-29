using System.Collections;
using System.Collections.Generic;
using System.IO.MemoryMappedFiles;
using System.Runtime.InteropServices;
using UnityEngine;

// 共享内存数据结构（与Go端保持一致）
[StructLayout(LayoutKind.Sequential, Pack = 1)]
public struct SharedMemoryData
{
    [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 32)]
    public string PlayerID;
    public float PositionX;
    public float PositionY;
    public float VelocityX;
    public float VelocityY;
    public float InputX;
    public float InputY;
    [MarshalAs(UnmanagedType.I1)]
    public bool IsJumping;
    [MarshalAs(UnmanagedType.I1)]
    public bool IsConnected;
    public uint FrameCounter;
}

public class PhysicsController : MonoBehaviour
{
    [Header("共享内存设置")]
    public string sharedMemoryName = "UnityGoSharedMemory";
    public int sharedMemorySize = 1024; // 1KB
    
    [Header("物理设置")]
    public float moveSpeed = 5f;
    public float jumpForce = 10f;
    public bool isGrounded = true;
    
    private MemoryMappedFile sharedMemory;
    private MemoryMappedViewAccessor accessor;
    private SharedMemoryData sharedData;
    private Rigidbody2D rb;
    
    void Start()
    {
        rb = GetComponent<Rigidbody2D>();
        
        // 初始化共享内存
        InitializeSharedMemory();
        
        Debug.Log("PhysicsController 初始化完成");
    }
    
    void InitializeSharedMemory()
    {
        try
        {
            // 尝试打开现有共享内存，如果不存在则创建
            sharedMemory = MemoryMappedFile.CreateOrOpen(sharedMemoryName, sharedMemorySize);
            accessor = sharedMemory.CreateViewAccessor();
            
            // 初始化共享内存数据
            sharedData = new SharedMemoryData
            {
                PlayerID = "unity_player",
                PositionX = transform.position.x,
                PositionY = transform.position.y,
                IsConnected = true,
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
    
    void Update()
    {
        // 读取用户输入
        HandleInput();
        
        // 写入输入数据到共享内存
        WriteInputToSharedMemory();
        
        // 从共享内存读取物理数据
        ReadPhysicsFromSharedMemory();
        
        // 更新Unity物体位置
        UpdateTransform();
    }
    
    void HandleInput()
    {
        // 读取键盘输入
        float horizontal = Input.GetAxis("Horizontal");
        float vertical = Input.GetAxis("Vertical");
        bool jump = Input.GetKeyDown(KeyCode.Space);
        
        // 本地物理模拟（可选，可以完全依赖Go物理引擎）
        if (horizontal != 0)
        {
            rb.velocity = new Vector2(horizontal * moveSpeed, rb.velocity.y);
        }
        
        if (jump && isGrounded)
        {
            rb.AddForce(new Vector2(0, jumpForce), ForceMode2D.Impulse);
            isGrounded = false;
        }
        
        // 更新共享数据
        sharedData.InputX = horizontal;
        sharedData.InputY = vertical;
        sharedData.IsJumping = jump;
    }
    
    void WriteInputToSharedMemory()
    {
        try
        {
            accessor.Write(0, ref sharedData);
            sharedData.FrameCounter++;
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
            
            // 使用Go物理引擎计算的位置（可选）
            // Vector2 goPosition = new Vector2(sharedData.PositionX, sharedData.PositionY);
            // transform.position = goPosition;
        }
        catch (System.Exception e)
        {
            Debug.LogError($"读取共享内存失败: {e.Message}");
        }
    }
    
    void UpdateTransform()
    {
        // 可以选择使用本地物理或Go物理引擎的结果
        // 这里使用本地物理，但同步共享内存数据
        sharedData.PositionX = transform.position.x;
        sharedData.PositionY = transform.position.y;
        sharedData.VelocityX = rb.velocity.x;
        sharedData.VelocityY = rb.velocity.y;
    }
    
    void OnCollisionEnter2D(Collision2D collision)
    {
        if (collision.gameObject.CompareTag("Ground"))
        {
            isGrounded = true;
        }
    }
    
    void OnApplicationQuit()
    {
        // 清理共享内存
        if (accessor != null)
        {
            sharedData.IsConnected = false;
            WriteToSharedMemory();
            accessor.Dispose();
        }
        if (sharedMemory != null)
        {
            sharedMemory.Dispose();
        }
        Debug.Log("PhysicsController 清理完成");
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
}
